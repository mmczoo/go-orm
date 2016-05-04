/*
MySQL的ORM框架，主要包含了通过反射将sql的Result映射成结构.
*/
package orm

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "github.com/go-sql-driver/mysql"
	"github.com/xlvector/dlog"
)

var sqlParamReg *regexp.Regexp
var initOnce sync.Once

func colName2FieldName(buf string) string {
	tks := strings.Split(buf, "_")
	ret := ""
	for _, tk := range tks {
		ret += strings.Title(tk)
	}
	return ret
}

func fieldName2ColName(buf string) string {
	w := bytes.Buffer{}
	for k, c := range buf {
		if unicode.IsUpper(c) {
			if k > 0 {
				w.WriteString("_")
			}
			w.WriteRune(unicode.ToLower(c))
		} else {
			w.WriteRune(c)
		}
	}
	return w.String()
}

func reflectStruct(s interface{}, cols []string, row *sql.Rows) error {
	v := reflect.ValueOf(s)
	return reflectStructValue(v, cols, row)
}

func reflectStructValue(v reflect.Value, cols []string, row *sql.Rows) error {
	if v.Kind() != reflect.Ptr {
		panic(errors.New("holder should be pointer"))
	}
	v = v.Elem()
	targets := make([]interface{}, len(cols))
	for k, c := range cols {
		fv := v.FieldByName(colName2FieldName(c))
		if !fv.CanAddr() {
			//log.Println("missing filed", c)
			var b interface{}
			targets[k] = &b
		} else {
			targets[k] = fv.Addr().Interface()
		}
	}
	err := row.Scan(targets...)
	if err != nil {
		return err
	}
	return nil
}

func checkStruct(s interface{}, cols []string, tableName string) error {

	v := reflect.TypeOf(s)

	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	for _, c := range cols {
		_, ok := v.FieldByName(colName2FieldName(c))
		if !ok {
			return errors.New(tableName + " missing field " + c)
		}
	}
	return nil
}

type Tdx interface {
	Exec(string, ...interface{}) (sql.Result, error)
	Query(string, ...interface{}) (*sql.Rows, error)
}

func getColumns(tdx Tdx, tableName string) ([]string, error) {
	ret := []string{}
	rows, err := tdx.Query("show columns from " + tableName)
	if err != nil {
		return ret, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, tp, nu, key, dft, extra sql.NullString
		if err := rows.Scan(&name, &tp, &nu, &key, &dft, &extra); err != nil {
			return ret, errors.New("can not scan filed:" + err.Error())
		}
		ret = append(ret, name.String)
	}
	if err := rows.Err(); err != nil {
		return ret, err
	}
	return ret, nil
}

func checkTableColumns(tdx Tdx, s interface{}) error {
	tableName := getTableName(s)
	cols, err := getColumns(tdx, tableName)
	if err != nil {
		return err
	}
	log.Println(tableName, cols)
	return checkStruct(s, cols, tableName)
}

func exec(tdx Tdx, query string, args ...interface{}) (sql.Result, error) {
	return tdx.Exec(query, args...)
}

func execWithParam(tdx Tdx, paramQuery string, paramMap interface{}) (sql.Result, error) {
	params := sqlParamReg.FindAllString(paramQuery, -1)
	if params != nil && len(params) > 0 {
		var args []interface{} = make([]interface{}, 0, len(params))
		for _, param := range params {
			param = param[2 : len(param)-1]
			value, err := getFieldValue(paramMap, param)
			if err != nil {
				return nil, err
			}
			args = append(args, value)
		}
		paramQuery = sqlParamReg.ReplaceAllLiteralString(paramQuery, "?")
		return tdx.Exec(paramQuery, args...)
	} else {
		dlog.Warn("no parameter found in paramQuery string")
		return tdx.Exec(paramQuery)
	}
}

func execWithRowAffectCheck(tdx Tdx, expectRows int64, query string, args ...interface{}) error {
	ret, err := tdx.Exec(query, args...)
	if err != nil {
		return err
	}
	ra, err := ret.RowsAffected()
	if err != nil {
		return err
	}
	if ra != expectRows {
		return errors.New(fmt.Sprintf("[RowAffectCheckError]: query [%s] should only affect %d rows, really affect %d rows", query, expectRows, ra))
	}
	return nil
}

func getPKColumn(s interface{}) string {
	t := reflect.TypeOf(s).Elem()
	return getPkColumnByType(t)
}

func getPkColumnByType(t reflect.Type) string {
	for k := 0; k < t.NumField(); k++ {
		ft := t.Field(k)
		if ft.Tag.Get("pk") == "true" {
			return fieldName2ColName(ft.Name)
		}
	}
	return ""
}

type orColumn struct {
	fieldName string
	or        string
	table     string
	orType    reflect.Type
}

func getOrColumns(s interface{}) (reflect.StructField, []*orColumn) {
	t := reflect.TypeOf(s).Elem()
	return getOrColumnsByType(t)
}

func getOrColumnsByType(t reflect.Type) (reflect.StructField, []*orColumn) {
	res := make([]*orColumn, 0)
	pkColumn := reflect.StructField{}
	// TODO: error check, i.e., has_one field must be a pointer of registered model
	for k := 0; k < t.NumField(); k++ {
		ft := t.Field(k)
		orTag := ft.Tag.Get("or")
		if orTag != "" {
			if orTag == "has_one" || orTag == "has_many" || orTag == "belongs_to" {
				var orType reflect.Type
				if orTag == "has_one" {
					if ft.Type.Kind() != reflect.Ptr {
						panic(errors.New(ft.Name + " should be pointer"))
					}
					orType = ft.Type.Elem()
				} else if orTag == "has_many" {
					if ft.Type.Kind() != reflect.Slice {
						panic(errors.New(ft.Name + " should be slice of pointer"))
					}
					elemType := ft.Type.Elem()
					if elemType.Kind() != reflect.Ptr {
						panic(errors.New(ft.Name + " should be slice of pointer"))
					}
					orType = elemType.Elem()
				} else if orTag == "belongs_to" {
					if ft.Type.Kind() != reflect.Ptr {
						panic(errors.New(ft.Name + " should be pointer"))
					}
					orType = ft.Type.Elem()
				}
				orTableName := ft.Tag.Get("table")
				if orTableName == "" {
					panic(errors.New("invalid table name in or tag on field: " + ft.Name))
				}
				res = append(res, &orColumn{
					fieldName: ft.Name,
					or:        orTag,
					table:     orTableName,
					orType:    orType,
				})
			} else {
				panic(errors.New("unsupported or tag: " + orTag + ", only support has_one, has_many and belongs_to for now"))
			}
		}
		if ft.Tag.Get("pk") == "true" {
			pkColumn = ft
		}
	}
	return pkColumn, res
}

func getTableName(s interface{}) string {
	ts := reflect.TypeOf(s)
	if ts.Kind() == reflect.Ptr {
		ts = ts.Elem()
	}
	return fieldName2ColName(ts.Name())
}

func selectByPK(tdx Tdx, s interface{}, pk interface{}) error {
	pkname := getPKColumn(s)
	tabname := getTableName(s)
	if pkname == "" {
		return errors.New(tabname + " does not have primary key")
	}
	return selectOne(tdx, s, fmt.Sprintf("select * from %s where %s = ?", tabname, pkname), pk)
}

func selectOne(tdx Tdx, s interface{}, query string, args ...interface{}) error {
	// One time there only can be one active sql Rows query
	err := selectOneInternal(tdx, s, query, args...)
	if err != nil {
		return err
	}
	pk, orColumns := getOrColumns(s)
	if orColumns != nil && len(orColumns) > 0 {
		v := reflect.ValueOf(s).Elem()
		pkValue, err := getFieldValue(s, pk.Name)
		if err != nil {
			return err
		}
		for _, orCol := range orColumns {
			if orCol.or == "has_one" {
				err = processOrHasOneRelation(tdx, orCol, v, pk, pkValue)
				if err != nil {
					return err
				}
			} else if orCol.or == "has_many" {
				orField := v.FieldByName(orCol.fieldName)
				err = selectManyInternal(tdx, orField.Addr().Interface(), false,
					"SELECT * FROM "+orCol.table+" WHERE "+fieldName2ColName(pk.Name)+" = ?", pkValue)
				if err != nil {
					return err
				}
			} else if orCol.or == "belongs_to" {
				fk := getPkColumnByType(orCol.orType)
				if fk == "" {
					panic(errors.New("error while getting primary key of " + orCol.table + " for belongs_to"))
				}
				fkValue, err := getFieldValue(s, colName2FieldName(fk))
				if err != nil {
					return err
				}
				err = processOrBelongsToRelation(tdx, orCol, v, fk, fkValue)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func selectOneInternal(tdx Tdx, s interface{}, query string, args ...interface{}) error {
	rows, err := tdx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return sql.ErrNoRows
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	err = reflectStruct(s, cols, rows)
	if err != nil {
		return err
	}

	return nil
}

func processOrHasOneRelation(tdx Tdx, orCol *orColumn, v reflect.Value, pk reflect.StructField, pkValue interface{}) error {
	orRows, err := tdx.Query("SELECT * FROM "+orCol.table+" WHERE "+fieldName2ColName(pk.Name)+" = ? LIMIT 1",
		pkValue)
	if err != nil {
		return err
	}
	defer orRows.Close()

	if !orRows.Next() {
		return nil
	}
	orCols, err := orRows.Columns()
	if err != nil {
		return err
	}
	orField := v.FieldByName(orCol.fieldName)
	orValue := reflect.New(orField.Type().Elem())
	err = reflectStructValue(orValue, orCols, orRows)
	if err != nil {
		return err
	}
	orField.Set(orValue)
	return nil
}

func processOrBelongsToRelation(tdx Tdx, orCol *orColumn, v reflect.Value, fk string, fkValue interface{}) error {
	orRows, err := tdx.Query("SELECT * FROM "+orCol.table+" WHERE "+fk+" = ? LIMIT 1",
		fkValue)
	if err != nil {
		return err
	}
	defer orRows.Close()

	if !orRows.Next() {
		return nil
	}
	orCols, err := orRows.Columns()
	if err != nil {
		return err
	}
	orField := v.FieldByName(orCol.fieldName)
	orValue := reflect.New(orField.Type().Elem())
	err = reflectStructValue(orValue, orCols, orRows)
	if err != nil {
		return err
	}
	orField.Set(orValue)
	return nil
}

func selectStr(tdx Tdx, query string, args ...interface{}) (string, error) {
	rows, err := tdx.Query(query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	if !rows.Next() {
		return "", sql.ErrNoRows
	}
	ret := ""
	err = rows.Scan(&ret)
	return ret, err
}

func selectInt(tdx Tdx, query string, args ...interface{}) (int64, error) {
	rows, err := tdx.Query(query, args...)
	var ret int64
	if err != nil {
		return ret, err
	}
	defer rows.Close()

	if !rows.Next() {
		return ret, sql.ErrNoRows
	}

	err = rows.Scan(&ret)
	return ret, err
}

func toSliceType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)
	if t.Kind() != reflect.Ptr {
		if t.Kind() == reflect.Slice {
			return nil, errors.New("can not select into a non-pointer slice")
		}
		return nil, nil
	}
	if t = t.Elem(); t.Kind() != reflect.Slice {
		return nil, errors.New("can not select into a non-pointer slice")
	}
	return t.Elem(), nil
}

func selectRawSet(tdx Tdx, query string, args ...interface{}) ([]map[string]string, error) {
	rows, err := tdx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dataSet := make([]map[string]string, 0, 1)

	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			return dataSet, err
		}
		itemMap := make(map[string]string)
		itemList := make([]interface{}, len(cols))
		for i := range itemList {
			itemList[i] = new(interface{})
		}

		err = rows.Scan(itemList...)

		if err != nil {
			log.Println("%v, %v", err, rows)
			return dataSet, err
		}
		for k, c := range cols {
			fname := colName2FieldName(c)
			switch t := (*itemList[k].(*interface{})).(type) {
			case []uint8:
				itemMap[fname] = string(t[:])
			case time.Time:
				itemMap[fname] = t.Format("2006-01-02 15:04:05")
			case int64:
				itemMap[fname] = strconv.FormatInt(t, 10)
			case int:
				itemMap[fname] = strconv.Itoa(t)
			case float32:
				itemMap[fname] = strconv.FormatFloat(float64(t), 'f', 4, 32)
			case float64:
				itemMap[fname] = strconv.FormatFloat(t, 'f', 4, 64)
			case nil:
			default:
			}
		}
		dataSet = append(dataSet, itemMap)
	}
	return dataSet, nil
}

func selectRaw(tdx Tdx, query string, args ...interface{}) ([]string, [][]string, error) {
	rows, err := tdx.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	colNames, err := rows.Columns()
	data := [][]string{}
	if err != nil {
		return colNames, data, err
	}

	for rows.Next() {
		itemMap := make([]string, len(colNames))
		itemList := make([]interface{}, len(colNames))
		for i := range itemList {
			itemList[i] = new(interface{})
		}

		err = rows.Scan(itemList...)

		if err != nil {
			log.Println("%v, %v", err, rows)
			return colNames, data, err
		}
		for k, _ := range colNames {
			switch t := (*itemList[k].(*interface{})).(type) {
			case []uint8:
				itemMap[k] = string(t[:])
			case time.Time:
				itemMap[k] = t.Format("2006-01-02 15:04:05")
			case int64:
				itemMap[k] = strconv.FormatInt(t, 10)
			case int:
				itemMap[k] = strconv.Itoa(t)
			case float32:
				itemMap[k] = strconv.FormatFloat(float64(t), 'f', 4, 32)
			case float64:
				itemMap[k] = strconv.FormatFloat(t, 'f', 4, 64)
			case nil:
			default:
			}
		}
		data = append(data, itemMap)
	}
	return colNames, data, nil
}

func selectMany(tdx Tdx, s interface{}, query string, args ...interface{}) error {
	return selectManyInternal(tdx, s, true, query, args...)
}

func selectManyInternal(tdx Tdx, s interface{}, processOr bool, query string, args ...interface{}) error {
	t, err := toSliceType(s)
	if err != nil {
		return err
	}

	if t.Kind() != reflect.Ptr && t.Kind() != reflect.Int64 && t.Kind() != reflect.String &&
		t.Kind() != reflect.Int && t.Kind() != reflect.Bool && t.Kind() != reflect.Float64 &&
		t.Kind() != reflect.Float32 && t.Kind() != reflect.Uint64 && t.Kind() != reflect.Uint {
		return errors.New("slice elements type " + t.Kind().String() + " not supported")
	}

	var isPtr = (t.Kind() == reflect.Ptr)

	hasOrCols := false
	pkCol := reflect.StructField{}
	var orCols []*orColumn = nil
	if isPtr {
		t = t.Elem()
		if processOr {
			pkCol, orCols = getOrColumnsByType(t)
			hasOrCols = orCols != nil && len(orCols) > 0
		}
	}

	sliceValue := reflect.Indirect(reflect.ValueOf(s))

	rows, err := tdx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	keys := make([]interface{}, 0)
	resMap := map[interface{}]reflect.Value{}
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		v := reflect.New(t)
		if isPtr {
			targets := make([]interface{}, len(cols))
			for k, c := range cols {
				fname := colName2FieldName(c)
				fv := v.Elem().FieldByName(fname)
				if !fv.CanAddr() {
					//fmt.Printf("missing field: %s , query: %s", fname, query)
					var b interface{}
					targets[k] = &b
					continue
				}
				targets[k] = fv.Addr().Interface()
			}
			err = rows.Scan(targets...)

			if err != nil {
				log.Println("%v, %v", err, rows)
				return err
			}
			sliceValue.Set(reflect.Append(sliceValue, v))
			if hasOrCols {
				pkFv := v.Elem().FieldByName(pkCol.Name)
				if pkFv.IsValid() {
					key := pkFv.Interface()
					keys = append(keys, key)
					resMap[key] = v
				}
			}
		} else {
			err = rows.Scan(v.Interface())
			if err != nil {
				return err
			}
			sliceValue.Set(reflect.Append(sliceValue, v.Elem()))
		}
	}
	if len(keys) > 0 {
		for _, orCol := range orCols {
			var sqlQuery string
			// 如果是belongs_to，需要先把fk -> array(elem)存下来，然后根据数据库请求结果将对应fk的指针指向相应的关联对象
			if orCol.or == "belongs_to" {
				fk := getPkColumnByType(orCol.orType)
				if fk == "" {
					return errors.New("error while getting primary key of " + orCol.table + " for belongs_to")
				}
				fkCol := colName2FieldName(fk)
				fkValues := make([]interface{}, 0)
				fkMaps := map[interface{}][]reflect.Value{}
				i := 0
				for _, value := range resMap {
					fkValue, err := getFieldValue(value.Interface(), fkCol)
					if err != nil {
						return err
					}
					fkValues = append(fkValues, fkValue)
					if v, ok := fkMaps[fkValue]; ok {
						fkMaps[fkValue] = append(v, value)
					} else {
						fkMaps[fkValue] = make([]reflect.Value, 0)
						fkMaps[fkValue] = append(fkMaps[fkValue], value)
					}
					i = i + 1
				}
				sqlQuery = makeString("SELECT * FROM "+orCol.table+" WHERE "+fk+" in (",
					",", ")", fkValues)
				orRows, err := tdx.Query(sqlQuery)

				if err != nil {
					return err
				}
				defer orRows.Close()
				for orRows.Next() {
					orCols, err := orRows.Columns()
					if err != nil {
						return err
					}
					orValue := reflect.New(orCol.orType)
					err = reflectStructValue(orValue, orCols, orRows)
					if err != nil {
						return err
					}
					keyValue := orValue.Elem().FieldByName(fkCol)
					if keyValue.IsValid() {
						if arr, ok := fkMaps[keyValue.Interface()]; ok {
							for _, v := range arr {
								v.Elem().FieldByName(orCol.fieldName).Set(orValue)
							}
						}

					}
				}
			} else {
				sqlQuery = makeString("SELECT * FROM "+orCol.table+" WHERE "+fieldName2ColName(pkCol.Name)+" in (",
					",", ")", keys)
				orRows, err := tdx.Query(sqlQuery)

				if err != nil {
					return err
				}
				defer orRows.Close()

				for orRows.Next() {
					orCols, err := orRows.Columns()
					if err != nil {
						return err
					}
					orValue := reflect.New(orCol.orType)
					err = reflectStructValue(orValue, orCols, orRows)
					if err != nil {
						return err
					}
					keyValue := orValue.Elem().FieldByName(pkCol.Name)
					if keyValue.IsValid() {
						if v, ok := resMap[keyValue.Interface()]; ok {
							if orCol.or == "has_one" {
								v.Elem().FieldByName(orCol.fieldName).Set(orValue)
							} else if orCol.or == "has_many" {
								orSliceValue := v.Elem().FieldByName(orCol.fieldName)
								orSliceValue.Set(reflect.Append(orSliceValue, orValue))
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func makeString(start, split, end string, ids []interface{}) string {
	buff := bytes.Buffer{}
	buff.WriteString(start)
	len := len(ids)
	for i, v := range ids {
		buff.WriteString(fmt.Sprintf("%v", v))
		if i < len-1 {
			buff.WriteString(split)
		}
	}
	buff.WriteString(end)
	return buff.String()
}

func columnsByStruct(s interface{}) (string, string, []interface{}, reflect.Value, bool) {
	t := reflect.TypeOf(s).Elem()
	v := reflect.ValueOf(s).Elem()
	cols := ""
	vals := ""
	ret := make([]interface{}, 0, t.NumField())
	n := 0
	var pk reflect.Value
	isAi := false
	for k := 0; k < t.NumField(); k++ {
		ft := t.Field(k)
		cn := fieldName2ColName(ft.Name)

		//auto increment field
		if ft.Tag.Get("pk") == "true" {
			pk = v.Field(k)
			if ft.Tag.Get("ai") == "true" {
				isAi = true
				continue
			}
		}

		//auto update filed, created_at, updated_at, etc.
		if ft.Tag.Get("ignore") == "true" || ft.Tag.Get("or") != "" {
			continue
		}

		if n > 0 {
			cols += ","
			vals += ","
		}
		cols += cn
		vals += "?"
		ret = append(ret, v.Field(k).Addr().Interface())
		n += 1
	}
	return cols, vals, ret, pk, isAi
}

func columnsBySlice(s []interface{}) (string, string, []interface{}, []reflect.Value, []bool) {
	t := reflect.TypeOf(s[0]).Elem()
	ret := make([]interface{}, 0, t.NumField()*len(s))
	cols := "("
	isFirst := true
	for k := 0; k < t.NumField(); k++ {
		ft := t.Field(k)
		cn := fieldName2ColName(ft.Name)
		if ft.Tag.Get("pk") == "true" {
			if ft.Tag.Get("ai") == "true" {
				continue
			}
		}
		if ft.Tag.Get("ignore") == "true" || ft.Tag.Get("or") != "" {
			continue
		}
		if !isFirst {
			cols += ","
		}
		cols += cn
		isFirst = false
	}
	cols += ")"

	vals := bytes.Buffer{}
	pks := make([]reflect.Value, len(s))
	ais := make([]bool, len(s))
	for n, record := range s {
		ct := reflect.TypeOf(record).Elem()
		if ct.Name() != t.Name() {
			continue
		}
		v := reflect.ValueOf(record).Elem()
		if n > 0 {
			vals.WriteString(",")
		}
		vals.WriteString("(")
		isFirst := true
		for k := 0; k < t.NumField(); k++ {
			ft := t.Field(k)

			//auto increment field
			if ft.Tag.Get("pk") == "true" {
				if ft.Tag.Get("ai") == "true" {
					pks[n] = v.Field(k)
					ais[n] = true
					continue
				}
			}

			//auto update filed, created_at, updated_at, etc.
			if ft.Tag.Get("ignore") == "true" || ft.Tag.Get("or") != "" {
				continue
			}

			if !isFirst {
				vals.WriteString(",")
			}
			vals.WriteString("?")
			isFirst = false
			ret = append(ret, v.Field(k).Addr().Interface())
		}
		vals.WriteString(")")
	}

	return cols, vals.String(), ret, pks, ais
}

func insert(tdx Tdx, s interface{}, ignore bool) error {
	cols, vals, ifs, pk, isAi := columnsByStruct(s)
	t := reflect.TypeOf(s).Elem()

	prefix := "insert"
	if ignore {
		prefix += " ignore"
	}
	q := fmt.Sprintf("%s into %s (%s) values(%s)", prefix, fieldName2ColName(t.Name()), cols, vals)
	ret, err := tdx.Exec(q, ifs...)
	if err != nil {
		return err
	}
	if isAi {
		lid, err := ret.LastInsertId()
		if err != nil {
			return err
		}
		if pk.Kind() == reflect.Int64 {
			pk.SetInt(lid)
		}
	}
	return nil
}

func insertBatch(tdx Tdx, s []interface{}, ignore bool) error {
	if s == nil || len(s) == 0 {
		return nil
	}
	//todo 需要check s中的数据都是同一种类型
	cols, vals, ifs, pks, ais := columnsBySlice(s)
	t := reflect.TypeOf(s[0]).Elem()

	prefix := "insert"
	if ignore {
		prefix += " ignore"
	}
	q := fmt.Sprintf("%s into %s %s values %s", prefix, fieldName2ColName(t.Name()), cols, vals)
	ret, err := tdx.Exec(q, ifs...)
	if err != nil {
		return err
	}
	//获取批量插入的last insert id, 然后给每个s[i]主键赋值
	lastInsertId, err := ret.LastInsertId()
	if err != nil {
		return err
	}
	for i, _ := range s {
		if ais[i] {
			pks[i].SetInt(lastInsertId + int64(i))
		}
	}
	return nil
}

type ORMer interface {
	SelectOne(interface{}, string, ...interface{}) error
	SelectByPK(interface{}, interface{}) error
	Select(interface{}, string, ...interface{}) error
	SelectStr(string, ...interface{}) (string, error)
	SelectInt(string, ...interface{}) (int64, error)
	Insert(interface{}, bool) error
	InsertBatch([]interface{}, bool) error
	Exec(string, ...interface{}) (sql.Result, error)
	ExecWithParam(string, interface{}) (sql.Result, error)
	ExecWithRowAffectCheck(int64, string, ...interface{}) error
}

type ORM struct {
	db     *sql.DB
	tables map[string]interface{}
}

func NewORM(ds string) *ORM {
	initOnce.Do(func() {
		sqlParamReg, _ = regexp.Compile("(#{[a-zA-Z0-9-_]*})")
	})
	ret := &ORM{
		db:     nil,
		tables: make(map[string]interface{}),
	}
	var err error
	ret.db, err = sql.Open("mysql", ds)
	if err != nil {
		log.Fatalln("can not connect to db:", err)
	}
	ret.db.SetMaxOpenConns(100)
	ret.db.SetMaxIdleConns(5)
	return ret
}

func (o *ORM) Close() error {
	return o.db.Close()
}

func (o *ORM) AddTable(s interface{}) {
	name := fieldName2ColName(reflect.TypeOf(s).Name())
	o.tables[name] = s
}

func (o *ORM) CheckTables() {
	for _, s := range o.tables {
		err := checkTableColumns(o.db, s)
		if err != nil {
			log.Fatalln("can not pass table check:", err)
		}
	}
}

func (o *ORM) GetTableByName(name string) interface{} {
	ret, ok := o.tables[name]
	if !ok {
		return nil
	} else {
		return ret
	}
}

func (o *ORM) TruncateTable(t string) error {
	_, err := o.db.Exec("truncate table " + t)
	return err
}

func (o *ORM) TruncateTables() error {
	for t, _ := range o.tables {
		err := o.TruncateTable(t)
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *ORM) Begin() (*ORMTran, error) {
	tx, err := o.db.Begin()
	return &ORMTran{tx: tx}, err
}

func (o *ORM) SelectOne(s interface{}, query string, args ...interface{}) error {
	return selectOne(o.db, s, query, args...)
}

func (o *ORM) SelectByPK(s interface{}, pk interface{}) error {
	return selectByPK(o.db, s, pk)
}

func (o *ORM) Select(s interface{}, query string, args ...interface{}) error {
	return selectMany(o.db, s, query, args...)
}

func (o *ORM) SelectRawSet(query string, args ...interface{}) ([]map[string]string, error) {
	return selectRawSet(o.db, query, args...)
}

func (o *ORM) SelectRaw(query string, args ...interface{}) ([]string, [][]string, error) {
	return selectRaw(o.db, query, args...)
}

func (o *ORM) SelectStr(query string, args ...interface{}) (string, error) {
	return selectStr(o.db, query, args...)
}

func (o *ORM) SelectInt(query string, args ...interface{}) (int64, error) {
	return selectInt(o.db, query, args...)
}

func (o *ORM) Insert(s interface{}, ignore bool) error {
	return insert(o.db, s, ignore)
}

func (o *ORM) InsertBatch(s []interface{}, ignore bool) error {
	return insertBatch(o.db, s, ignore)
}

func (o *ORM) ExecWithRowAffectCheck(n int64, query string, args ...interface{}) error {
	return execWithRowAffectCheck(o.db, n, query, args...)
}

func (o *ORM) Exec(query string, args ...interface{}) (sql.Result, error) {
	return exec(o.db, query, args...)
}

func (o *ORM) ExecWithParam(paramQuery string, paramMap interface{}) (sql.Result, error) {
	return execWithParam(o.db, paramQuery, paramMap)
}

func getFieldValue(param interface{}, fieldName string) (interface{}, error) {
	v := reflect.ValueOf(param)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() == reflect.Map {
		fv := reflect.ValueOf(fieldName)
		f := v.MapIndex(fv)
		if f.IsValid() {
			return f.Interface(), nil
		} else {
			return nil, errors.New("missing field " + fieldName)
		}
	} else if v.Kind() == reflect.Struct {
		f := v.FieldByName(fieldName)
		var x []int
		x = append(x, 6)
		if f.IsValid() {
			return f.Interface(), nil
		} else {
			return nil, errors.New("missing field " + fieldName)
		}
	} else {
		return nil, errors.New(fmt.Sprintf("input interface type {%v} is not supported", v.Kind().String()))
	}
}

func (o *ORM) DoTransaction(f func(*ORMTran) error) error {
	trans, err := o.Begin()
	if err != nil {
		return err
	}
	defer func() {
		perr := recover()
		if err != nil || perr != nil {
			trans.Rollback()
			if perr != nil {
				panic(perr)
			}
			return
		} else {
			err = trans.Commit()
			return
		}
	}()
	err = f(trans)
	return err
}

func (o *ORM) DoTransactionMore(f func(*ORMTran) (interface{}, error)) (interface{}, error) {
	trans, err := o.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			trans.Rollback()
			return
		} else {
			err = trans.Commit()
			return
		}
	}()
	return f(trans)
}

type ORMTran struct {
	tx *sql.Tx
}

func (o *ORMTran) SelectOne(s interface{}, query string, args ...interface{}) error {
	return selectOne(o.tx, s, query, args...)
}

func (o *ORMTran) Insert(s interface{}, ignore bool) error {
	return insert(o.tx, s, ignore)
}

func (o *ORMTran) InsertBatch(s []interface{}, ignore bool) error {
	return insertBatch(o.tx, s, ignore)
}

func (o *ORMTran) Exec(query string, args ...interface{}) (sql.Result, error) {
	return exec(o.tx, query, args...)
}

func (o *ORMTran) Commit() error {
	return o.tx.Commit()
}

func (o *ORMTran) Rollback() error {
	return o.tx.Rollback()
}

func (o *ORMTran) SelectByPK(s interface{}, pk interface{}) error {
	return selectByPK(o.tx, s, pk)
}

func (o *ORMTran) Select(s interface{}, query string, args ...interface{}) error {
	return selectMany(o.tx, s, query, args...)
}

func (o *ORMTran) SelectInt(query string, args ...interface{}) (int64, error) {
	return selectInt(o.tx, query, args...)
}

func (o *ORMTran) SelectStr(query string, args ...interface{}) (string, error) {
	return selectStr(o.tx, query, args...)
}

func (o *ORMTran) ExecWithParam(paramQuery string, paramMap interface{}) (sql.Result, error) {
	return execWithParam(o.tx, paramQuery, paramMap)
}

func (o *ORMTran) ExecWithRowAffectCheck(n int64, query string, args ...interface{}) error {
	return execWithRowAffectCheck(o.tx, n, query, args...)
}

func IsRowAffectError(err error) bool {
	return strings.HasPrefix(err.Error(), "[RowAffectCheckError]")
}
