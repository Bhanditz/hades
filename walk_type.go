package hades

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/pkg/errors"
)

type JoinRec struct {
	DestinKey interface{}
	Record    reflect.Value
}

type ManyToMany struct {
	Scope *Scope

	JoinTable string

	SourceName        string
	SourceAssocName   string
	SourceDBName      string
	SourceAssocDBName string

	DestinName        string
	DestinAssocName   string
	DestinDBName      string
	DestinAssocDBName string

	// SourceKey => []JoinRec{DestinKey, Record}
	Values map[interface{}][]JoinRec
}

func (c *Context) NewManyToMany(JoinTable string, SourceForeignKeys, DestinationForeignKeys []JoinTableForeignKey) (*ManyToMany, error) {
	scope := c.ScopeMap.ByDBName(JoinTable)
	if scope == nil {
		return nil, fmt.Errorf("Could not find model struct for %s: list it explicitly in Models", JoinTable)
	}

	if len(SourceForeignKeys) != 1 {
		return nil, fmt.Errorf("For join table %s, expected 1 source foreign keys but got %d",
			JoinTable, len(SourceForeignKeys))
	}
	if len(DestinationForeignKeys) != 1 {
		return nil, fmt.Errorf("For join table %s, expected 1 destination foreign keys but got %d",
			JoinTable, len(DestinationForeignKeys))
	}

	sfk := SourceForeignKeys[0]
	dfk := DestinationForeignKeys[0]

	mtm := &ManyToMany{
		JoinTable: JoinTable,
		Scope:     scope,

		SourceName:        FromDBName(sfk.DBName),
		SourceAssocName:   FromDBName(sfk.AssociationDBName),
		SourceDBName:      sfk.DBName,
		SourceAssocDBName: sfk.AssociationDBName,

		DestinName:        FromDBName(dfk.DBName),
		DestinAssocName:   FromDBName(dfk.AssociationDBName),
		DestinDBName:      dfk.DBName,
		DestinAssocDBName: dfk.AssociationDBName,

		Values: make(map[interface{}][]JoinRec),
	}
	return mtm, nil
}

func (mtm *ManyToMany) Add(Source reflect.Value, Destin reflect.Value) {
	sourceKey := Source.Elem().FieldByName(mtm.SourceAssocName).Interface()
	destinKey := Destin.Elem().FieldByName(mtm.DestinAssocName).Interface()
	mtm.Values[sourceKey] = append(mtm.Values[sourceKey], JoinRec{
		DestinKey: destinKey,
	})
}

func (mtm *ManyToMany) AddKeys(sourceKey interface{}, destinKey interface{}, record reflect.Value) {
	mtm.Values[sourceKey] = append(mtm.Values[sourceKey], JoinRec{
		DestinKey: destinKey,
		Record:    record,
	})
}

func (mtm *ManyToMany) String() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("JoinTable: %s", mtm.JoinTable))
	lines = append(lines, fmt.Sprintf("SourceForeignKey: %s / %s", mtm.SourceName, mtm.SourceAssocName))
	lines = append(lines, fmt.Sprintf("DestinForeignKey: %s / %s", mtm.DestinName, mtm.DestinAssocName))
	for sourceKey, destinKeys := range mtm.Values {
		lines = append(lines, fmt.Sprintf("SourceKey %v", sourceKey))
		for _, destinKey := range destinKeys {
			lines = append(lines, fmt.Sprintf("  - DestinKey %v", destinKey))
		}
	}
	return strings.Join(lines, "\n")
}

type RecordInfo struct {
	Name         string
	Type         reflect.Type
	Children     []*RecordInfo
	Relationship *Relationship
	ManyToMany   *ManyToMany
	ModelStruct  *ModelStruct
}

func (ri *RecordInfo) String() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("- %s: %s", ri.Name, ri.Type.String()))
	for _, c := range ri.Children {
		for _, cl := range strings.Split(c.String(), "\n") {
			lines = append(lines, "  "+cl)
		}
	}
	return strings.Join(lines, "\n")
}

type VisitMap map[*ModelStruct]bool

func (vm VisitMap) CopyAndMark(ms *ModelStruct) VisitMap {
	vv := make(VisitMap)
	for k, v := range vm {
		vv[k] = v
	}
	vv[ms] = true
	return vv
}

type RecordInfoMap map[reflect.Type]*RecordInfo

func (c *Context) WalkType(riMap RecordInfoMap, name string, atyp reflect.Type, visited VisitMap, assocs []string) (*RecordInfo, error) {
	if atyp.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("WalkType expects a *Model type, got %v", atyp)
	}
	if atyp.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("WalkType expects a *Model type, got %v", atyp)
	}

	scope := c.ScopeMap.ByType(atyp)
	if scope == nil {
		return nil, fmt.Errorf("WalkType expects a *Model but %v is not a registered model type", atyp)
	}
	ms := scope.GetModelStruct()

	if visited[ms] {
		return nil, nil
	}
	visited = visited.CopyAndMark(ms)

	ri := &RecordInfo{
		Type:        atyp,
		Name:        name,
		ModelStruct: ms,
	}

	visitField := func(sf *StructField, explicit bool) error {
		if sf.Relationship == nil {
			if explicit {
				return fmt.Errorf("%s.%s does not describe a relationship", ms.ModelType.Name(), sf.Name)
			}
			return nil
		}

		fieldTyp := sf.Struct.Type
		if fieldTyp.Kind() == reflect.Slice {
			fieldTyp = fieldTyp.Elem()
		}
		if fieldTyp.Kind() != reflect.Ptr {
			return fmt.Errorf("visitField expects a Slice of Ptr, or a Ptr, but got %v", sf.Struct.Type)
		}

		if c.ScopeMap.ByType(fieldTyp) != nil {
			if explicit {
				return fmt.Errorf("%s.%s is not an explicitly listed model (%v)", ms.ModelType.Name(), sf.Name, fieldTyp)
			}
			return nil
		}

		child, err := c.WalkType(riMap, sf.Name, fieldTyp, visited, nil)
		if err != nil {
			return errors.Wrap(err, "walking type of child")
		}

		if child == nil {
			return nil
		}

		child.Relationship = sf.Relationship

		if sf.Relationship.Kind == "many_to_many" {
			jth := sf.Relationship.JoinTableHandler
			djth, ok := jth.(*JoinTableHandler)
			if !ok {
				return errors.Errorf("Expected sf.Relationship.JoinTableHandler to be the default JoinTableHandler type, but it's %v", reflect.TypeOf(jth))
			}

			mtm, err := c.NewManyToMany(djth.TableName, jth.SourceForeignKeys(), jth.DestinationForeignKeys())
			if err != nil {
				return errors.Wrap(err, "creating ManyToMany relation")
			}
			child.ManyToMany = mtm
		}

		ri.Children = append(ri.Children, child)
		return nil
	}

	if len(assocs) > 0 {
		sfByName := make(map[string]*StructField)
		for _, sf := range ms.StructFields {
			sfByName[sf.Name] = sf
		}

		// visit specified fields
		for _, fieldName := range assocs {
			sf, ok := sfByName[fieldName]
			if !ok {
				return nil, fmt.Errorf("No field '%s' in %s", fieldName, atyp)
			}
			err := visitField(sf, true)
			if err != nil {
				return nil, errors.Wrapf(err, "visiting field %s", fieldName)
			}
		}
	} else {
		// visit all fields
		for _, sf := range ms.StructFields {
			err := visitField(sf, false)
			if err != nil {
				return nil, errors.Wrapf(err, "visiting field %s", sf.Name)
			}
		}
	}

	riMap[atyp] = ri
	return ri, nil
}
