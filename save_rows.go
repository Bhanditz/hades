package hades

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/go-xorm/builder"

	"crawshaw.io/sqlite"
	"github.com/pkg/errors"
)

func (c *Context) saveRows(conn *sqlite.Conn, params *SaveParams, inputIface interface{}) error {
	// inputIFace is a `[]interface{}`
	input := reflect.ValueOf(inputIface)
	if input.Kind() != reflect.Slice {
		return errors.New("diff needs a slice")
	}

	if input.Len() == 0 {
		return nil
	}

	// we're trying to make fresh a `[]*SomeModel` slice, so
	// that scope can get annotation information from SomeModel struct annotations
	first := input.Index(0).Elem()
	fresh := reflect.MakeSlice(reflect.SliceOf(first.Type()), input.Len(), input.Len())
	for i := 0; i < input.Len(); i++ {
		record := input.Index(i).Elem()
		fresh.Index(i).Set(record)
	}

	// use scope to find the primary keys
	scope := c.NewScope(first.Interface())
	modelName := scope.GetModelStruct().ModelType.Name()
	primaryFields := scope.PrimaryFields()

	// this will happen for associations
	if len(primaryFields) != 1 {
		if len(primaryFields) != 2 {
			return fmt.Errorf("Have %d primary keys for %s, don't know what to do", len(primaryFields), modelName)
		}

		recordsGroupedByPrimaryField := make(map[*Field]map[interface{}][]reflect.Value)

		for _, primaryField := range primaryFields {
			recordsByKey := make(map[interface{}][]reflect.Value)

			for i := 0; i < fresh.Len(); i++ {
				rec := fresh.Index(i)
				key := rec.Elem().FieldByName(primaryField.Name).Interface()
				recordsByKey[key] = append(recordsByKey[key], rec)
			}
			recordsGroupedByPrimaryField[primaryField] = recordsByKey
		}

		var bestSourcePrimaryField *Field
		var bestNumGroups int64 = math.MaxInt64
		var valueMap map[interface{}][]reflect.Value
		for primaryField, recs := range recordsGroupedByPrimaryField {
			numGroups := len(recs)
			if int64(numGroups) < bestNumGroups {
				bestSourcePrimaryField = primaryField
				bestNumGroups = int64(numGroups)
				valueMap = recs
			}
		}

		if bestSourcePrimaryField == nil {
			return fmt.Errorf("Have %d primary keys for %s, don't know what to do", len(primaryFields), modelName)
		}

		var bestDestinPrimaryField *Field
		for primaryField := range recordsGroupedByPrimaryField {
			if primaryField != bestSourcePrimaryField {
				bestDestinPrimaryField = primaryField
				break
			}
		}
		if bestDestinPrimaryField == nil {
			return errors.New("Internal error: could not find bestDestinPrimaryField")
		}

		sourceRelField, ok := scope.FieldByName(strings.TrimSuffix(bestSourcePrimaryField.Name, "ID"))
		if !ok {
			return fmt.Errorf("Could not find assoc for %s.%s", modelName, bestSourcePrimaryField.Name)
		}
		destinRelField, ok := scope.FieldByName(strings.TrimSuffix(bestDestinPrimaryField.Name, "ID"))
		if !ok {
			return fmt.Errorf("Could not find assoc for %s.%s", modelName, bestDestinPrimaryField.Name)
		}

		sourceScope := c.ScopeMap.ByType(sourceRelField.Struct.Type)
		if sourceScope == nil {
			return fmt.Errorf("Could not find scope for assoc for %s.%s", modelName, bestSourcePrimaryField.Name)
		}
		destinScope := c.ScopeMap.ByType(destinRelField.Struct.Type)
		if destinScope == nil {
			return fmt.Errorf("Could not find scope for assoc for %s.%s", modelName, bestSourcePrimaryField.Name)
		}

		if len(sourceScope.PrimaryFields()) != 1 {
			return fmt.Errorf("Expected Source model %s to have 1 primary field, but it has %d",
				sourceScope.GetModelStruct().ModelType, len(sourceScope.PrimaryFields()))
		}
		if len(destinScope.PrimaryFields()) != 1 {
			return fmt.Errorf("Expected Destin model %s to have 1 primary field, but it has %d",
				destinScope.GetModelStruct().ModelType, len(destinScope.PrimaryFields()))
		}

		sourceJTFK := JoinTableForeignKey{
			DBName:            ToDBName(bestSourcePrimaryField.Name),
			AssociationDBName: sourceScope.PrimaryField().DBName,
		}

		destinJTFK := JoinTableForeignKey{
			DBName:            ToDBName(bestDestinPrimaryField.Name),
			AssociationDBName: destinScope.PrimaryField().DBName,
		}

		mtm, err := c.NewManyToMany(
			scope.TableName(),
			[]JoinTableForeignKey{sourceJTFK},
			[]JoinTableForeignKey{destinJTFK},
		)
		if err != nil {
			return errors.Wrap(err, "creating ManyToMany relationship")
		}

		for sourceKey, recs := range valueMap {
			for _, rec := range recs {
				destinKey := rec.Elem().FieldByName(bestDestinPrimaryField.Name).Interface()
				mtm.AddKeys(sourceKey, destinKey, rec)
			}
		}

		err = c.saveJoins(params, conn, mtm)
		if err != nil {
			return errors.Wrap(err, "saving joins")
		}

		return nil
	}

	primaryField := primaryFields[0]

	// record should be a *SomeModel, we're effectively doing (*record).<pkColumn>
	getKey := func(record reflect.Value) interface{} {
		f := record.Elem().FieldByName(primaryField.Name)
		if !f.IsValid() {
			return nil
		}
		return f.Interface()
	}

	// collect primary key values for all of input
	var keys []interface{}
	for i := 0; i < fresh.Len(); i++ {
		record := fresh.Index(i)
		keys = append(keys, getKey(record))
	}

	cacheAddr, err := c.pagedByKeys(conn, primaryField.DBName, keys, fresh.Type(), nil)
	if err != nil {
		return errors.Wrap(err, "getting existing rows")
	}

	cache := cacheAddr.Elem()

	// index cached items by their primary key
	// so we can look them up in O(1) when comparing
	cacheByPK := make(map[interface{}]reflect.Value)
	for i := 0; i < cache.Len(); i++ {
		record := cache.Index(i)
		cacheByPK[getKey(record)] = record
	}

	// compare cached records with fresh records
	var inserts []reflect.Value
	var updates = make(map[interface{}]ChangedFields)

	doneKeys := make(map[interface{}]bool)
	for i := 0; i < fresh.Len(); i++ {
		frec := fresh.Index(i)
		key := getKey(frec)
		if _, ok := doneKeys[key]; ok {
			continue
		}
		doneKeys[key] = true

		if crec, ok := cacheByPK[key]; ok {
			// frec and crec are *SomeModel, but `RecordEqual` ignores pointer
			// equality - we want to compare the contents of the struct
			// so we indirect to SomeModel here.
			ifrec := frec.Elem().Interface()
			icrec := crec.Elem().Interface()

			cf, err := DiffRecord(ifrec, icrec, scope)
			if err != nil {
				return errors.Wrap(err, "diffing db records")
			}

			if cf != nil {
				updates[key] = cf
			}
		} else {
			inserts = append(inserts, frec)
		}
	}

	c.Stats.Inserts += int64(len(inserts))
	c.Stats.Updates += int64(len(updates))
	c.Stats.Current += int64(fresh.Len() - len(updates) - len(inserts))

	if len(inserts) > 0 {
		for _, rec := range inserts {
			// FIXME: that's slow/bad because of ToEq
			err := c.Insert(conn, rec)
			if err != nil {
				return errors.Wrap(err, "inserting new DB records")
			}
		}
	}

	for key, rec := range updates {
		// FIXME: that's slow/bad
		eq := make(builder.Eq)
		for k, v := range rec {
			eq[ToDBName(k)] = v
		}
		err := c.Exec(conn, builder.Update(eq).Into(scope.TableName()).Where(builder.Eq{primaryField.DBName: key}), nil)
		if err != nil {
			return errors.Wrap(err, "updating DB records")
		}
	}

	return nil
}
