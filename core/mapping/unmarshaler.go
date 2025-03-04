package mapping

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/jsonx"
	"github.com/zeromicro/go-zero/core/lang"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/stringx"
)

const (
	defaultKeyName = "key"
	delimiter      = '.'
)

var (
	errTypeMismatch     = errors.New("type mismatch")
	errValueNotSettable = errors.New("value is not settable")
	errValueNotStruct   = errors.New("value type is not struct")
	keyUnmarshaler      = NewUnmarshaler(defaultKeyName)
	durationType        = reflect.TypeOf(time.Duration(0))
	cacheKeys           = make(map[string][]string)
	cacheKeysLock       sync.Mutex
	defaultCache        = make(map[string]interface{})
	defaultCacheLock    sync.Mutex
	emptyMap            = map[string]interface{}{}
	emptyValue          = reflect.ValueOf(lang.Placeholder)
)

type (
	// Unmarshaler is used to unmarshal with given tag key.
	Unmarshaler struct {
		key  string
		opts unmarshalOptions
	}

	// UnmarshalOption defines the method to customize an Unmarshaler.
	UnmarshalOption func(*unmarshalOptions)

	unmarshalOptions struct {
		fromString   bool
		canonicalKey func(key string) string
	}
)

// NewUnmarshaler returns an Unmarshaler.
func NewUnmarshaler(key string, opts ...UnmarshalOption) *Unmarshaler {
	unmarshaler := Unmarshaler{
		key: key,
	}

	for _, opt := range opts {
		opt(&unmarshaler.opts)
	}

	return &unmarshaler
}

// UnmarshalKey unmarshals m into v with tag key.
func UnmarshalKey(m map[string]interface{}, v interface{}) error {
	return keyUnmarshaler.Unmarshal(m, v)
}

// Unmarshal unmarshals m into v.
func (u *Unmarshaler) Unmarshal(m map[string]interface{}, v interface{}) error {
	return u.UnmarshalValuer(mapValuer(m), v)
}

// UnmarshalValuer unmarshals m into v.
func (u *Unmarshaler) UnmarshalValuer(m Valuer, v interface{}) error {
	return u.unmarshalWithFullName(simpleValuer{current: m}, v, "")
}

func (u *Unmarshaler) fillMap(fieldType reflect.Type, value reflect.Value, mapValue interface{}) error {
	if !value.CanSet() {
		return errValueNotSettable
	}

	fieldKeyType := fieldType.Key()
	fieldElemType := fieldType.Elem()
	targetValue, err := u.generateMap(fieldKeyType, fieldElemType, mapValue)
	if err != nil {
		return err
	}

	if !targetValue.Type().AssignableTo(value.Type()) {
		return errTypeMismatch
	}

	value.Set(targetValue)
	return nil
}

func (u *Unmarshaler) fillMapFromString(value reflect.Value, mapValue interface{}) error {
	if !value.CanSet() {
		return errValueNotSettable
	}

	switch v := mapValue.(type) {
	case fmt.Stringer:
		if err := jsonx.UnmarshalFromString(v.String(), value.Addr().Interface()); err != nil {
			return err
		}
	case string:
		if err := jsonx.UnmarshalFromString(v, value.Addr().Interface()); err != nil {
			return err
		}
	default:
		return errUnsupportedType
	}

	return nil
}

func (u *Unmarshaler) fillSlice(fieldType reflect.Type, value reflect.Value, mapValue interface{}) error {
	if !value.CanSet() {
		return errValueNotSettable
	}

	baseType := fieldType.Elem()
	baseKind := baseType.Kind()
	dereffedBaseType := Deref(baseType)
	dereffedBaseKind := dereffedBaseType.Kind()
	refValue := reflect.ValueOf(mapValue)
	if refValue.IsNil() {
		return nil
	}

	conv := reflect.MakeSlice(reflect.SliceOf(baseType), refValue.Len(), refValue.Cap())
	if refValue.Len() == 0 {
		value.Set(conv)
		return nil
	}

	var valid bool
	for i := 0; i < refValue.Len(); i++ {
		ithValue := refValue.Index(i).Interface()
		if ithValue == nil {
			continue
		}

		valid = true
		switch dereffedBaseKind {
		case reflect.Struct:
			target := reflect.New(dereffedBaseType)
			if err := u.Unmarshal(ithValue.(map[string]interface{}), target.Interface()); err != nil {
				return err
			}

			if baseKind == reflect.Ptr {
				conv.Index(i).Set(target)
			} else {
				conv.Index(i).Set(target.Elem())
			}
		case reflect.Slice:
			if err := u.fillSlice(dereffedBaseType, conv.Index(i), ithValue); err != nil {
				return err
			}
		default:
			if err := u.fillSliceValue(conv, i, dereffedBaseKind, ithValue); err != nil {
				return err
			}
		}
	}

	if valid {
		value.Set(conv)
	}

	return nil
}

func (u *Unmarshaler) fillSliceFromString(fieldType reflect.Type, value reflect.Value,
	mapValue interface{}) error {
	var slice []interface{}
	switch v := mapValue.(type) {
	case fmt.Stringer:
		if err := jsonx.UnmarshalFromString(v.String(), &slice); err != nil {
			return err
		}
	case string:
		if err := jsonx.UnmarshalFromString(v, &slice); err != nil {
			return err
		}
	default:
		return errUnsupportedType
	}

	baseFieldType := Deref(fieldType.Elem())
	baseFieldKind := baseFieldType.Kind()
	conv := reflect.MakeSlice(reflect.SliceOf(baseFieldType), len(slice), cap(slice))

	for i := 0; i < len(slice); i++ {
		if err := u.fillSliceValue(conv, i, baseFieldKind, slice[i]); err != nil {
			return err
		}
	}

	value.Set(conv)
	return nil
}

func (u *Unmarshaler) fillSliceValue(slice reflect.Value, index int,
	baseKind reflect.Kind, value interface{}) error {
	ithVal := slice.Index(index)
	switch v := value.(type) {
	case fmt.Stringer:
		return setValue(baseKind, ithVal, v.String())
	case string:
		return setValue(baseKind, ithVal, v)
	case map[string]interface{}:
		return u.fillMap(ithVal.Type(), ithVal, value)
	default:
		// don't need to consider the difference between int, int8, int16, int32, int64,
		// uint, uint8, uint16, uint32, uint64, because they're handled as json.Number.
		if ithVal.Kind() == reflect.Ptr {
			baseType := Deref(ithVal.Type())
			if !reflect.TypeOf(value).AssignableTo(baseType) {
				return errTypeMismatch
			}

			target := reflect.New(baseType).Elem()
			target.Set(reflect.ValueOf(value))
			ithVal.Set(target.Addr())
			return nil
		}

		if !reflect.TypeOf(value).AssignableTo(ithVal.Type()) {
			return errTypeMismatch
		}

		ithVal.Set(reflect.ValueOf(value))
		return nil
	}
}

func (u *Unmarshaler) fillSliceWithDefault(derefedType reflect.Type, value reflect.Value,
	defaultValue string) error {
	baseFieldType := Deref(derefedType.Elem())
	baseFieldKind := baseFieldType.Kind()
	defaultCacheLock.Lock()
	slice, ok := defaultCache[defaultValue]
	defaultCacheLock.Unlock()
	if !ok {
		if baseFieldKind == reflect.String {
			slice = parseGroupedSegments(defaultValue)
		} else if err := jsonx.UnmarshalFromString(defaultValue, &slice); err != nil {
			return err
		}

		defaultCacheLock.Lock()
		defaultCache[defaultValue] = slice
		defaultCacheLock.Unlock()
	}

	return u.fillSlice(derefedType, value, slice)
}

func (u *Unmarshaler) generateMap(keyType, elemType reflect.Type, mapValue interface{}) (reflect.Value, error) {
	mapType := reflect.MapOf(keyType, elemType)
	valueType := reflect.TypeOf(mapValue)
	if mapType == valueType {
		return reflect.ValueOf(mapValue), nil
	}

	refValue := reflect.ValueOf(mapValue)
	targetValue := reflect.MakeMapWithSize(mapType, refValue.Len())
	fieldElemKind := elemType.Kind()
	dereffedElemType := Deref(elemType)
	dereffedElemKind := dereffedElemType.Kind()

	for _, key := range refValue.MapKeys() {
		keythValue := refValue.MapIndex(key)
		keythData := keythValue.Interface()

		switch dereffedElemKind {
		case reflect.Slice:
			target := reflect.New(dereffedElemType)
			if err := u.fillSlice(elemType, target.Elem(), keythData); err != nil {
				return emptyValue, err
			}

			targetValue.SetMapIndex(key, target.Elem())
		case reflect.Struct:
			keythMap, ok := keythData.(map[string]interface{})
			if !ok {
				return emptyValue, errTypeMismatch
			}

			target := reflect.New(dereffedElemType)
			if err := u.Unmarshal(keythMap, target.Interface()); err != nil {
				return emptyValue, err
			}

			if fieldElemKind == reflect.Ptr {
				targetValue.SetMapIndex(key, target)
			} else {
				targetValue.SetMapIndex(key, target.Elem())
			}
		case reflect.Map:
			keythMap, ok := keythData.(map[string]interface{})
			if !ok {
				return emptyValue, errTypeMismatch
			}

			innerValue, err := u.generateMap(elemType.Key(), elemType.Elem(), keythMap)
			if err != nil {
				return emptyValue, err
			}

			targetValue.SetMapIndex(key, innerValue)
		default:
			switch v := keythData.(type) {
			case bool:
				if dereffedElemKind != reflect.Bool {
					return emptyValue, errTypeMismatch
				}

				targetValue.SetMapIndex(key, reflect.ValueOf(v))
			case string:
				if dereffedElemKind != reflect.String {
					return emptyValue, errTypeMismatch
				}

				targetValue.SetMapIndex(key, reflect.ValueOf(v))
			case json.Number:
				target := reflect.New(dereffedElemType)
				if err := setValue(dereffedElemKind, target.Elem(), v.String()); err != nil {
					return emptyValue, err
				}

				targetValue.SetMapIndex(key, target.Elem())
			default:
				if dereffedElemKind != keythValue.Kind() {
					return emptyValue, errTypeMismatch
				}

				targetValue.SetMapIndex(key, keythValue)
			}
		}
	}

	return targetValue, nil
}

func (u *Unmarshaler) parseOptionsWithContext(field reflect.StructField, m Valuer, fullName string) (
	string, *fieldOptionsWithContext, error) {
	key, options, err := parseKeyAndOptions(u.key, field)
	if err != nil {
		return "", nil, err
	} else if options == nil {
		return key, nil, nil
	}

	optsWithContext, err := options.toOptionsWithContext(key, m, fullName)
	if err != nil {
		return "", nil, err
	}

	return key, optsWithContext, nil
}

func (u *Unmarshaler) processAnonymousField(field reflect.StructField, value reflect.Value,
	m valuerWithParent, fullName string) error {
	key, options, err := u.parseOptionsWithContext(field, m, fullName)
	if err != nil {
		return err
	}

	if _, hasValue := getValue(m, key); hasValue {
		return fmt.Errorf("fields of %s can't be wrapped inside, because it's anonymous", key)
	}

	if options.optional() {
		return u.processAnonymousFieldOptional(field.Type, value, key, m, fullName)
	}

	return u.processAnonymousFieldRequired(field.Type, value, m, fullName)
}

func (u *Unmarshaler) processAnonymousFieldOptional(fieldType reflect.Type, value reflect.Value,
	key string, m valuerWithParent, fullName string) error {
	var filled bool
	var required int
	var requiredFilled int
	var indirectValue reflect.Value
	derefedFieldType := Deref(fieldType)

	for i := 0; i < derefedFieldType.NumField(); i++ {
		subField := derefedFieldType.Field(i)
		fieldKey, fieldOpts, err := u.parseOptionsWithContext(subField, m, fullName)
		if err != nil {
			return err
		}

		_, hasValue := getValue(m, fieldKey)
		if hasValue {
			if !filled {
				filled = true
				maybeNewValue(fieldType, value)
				indirectValue = reflect.Indirect(value)
			}
			if err = u.processField(subField, indirectValue.Field(i), m, fullName); err != nil {
				return err
			}
		}
		if !fieldOpts.optional() {
			required++
			if hasValue {
				requiredFilled++
			}
		}
	}

	if filled && required != requiredFilled {
		return fmt.Errorf("%s is not fully set", key)
	}

	return nil
}

func (u *Unmarshaler) processAnonymousFieldRequired(fieldType reflect.Type, value reflect.Value,
	m valuerWithParent, fullName string) error {
	maybeNewValue(fieldType, value)
	derefedFieldType := Deref(fieldType)
	indirectValue := reflect.Indirect(value)

	for i := 0; i < derefedFieldType.NumField(); i++ {
		if err := u.processField(derefedFieldType.Field(i), indirectValue.Field(i), m, fullName); err != nil {
			return err
		}
	}

	return nil
}

func (u *Unmarshaler) processField(field reflect.StructField, value reflect.Value,
	m valuerWithParent, fullName string) error {
	if usingDifferentKeys(u.key, field) {
		return nil
	}

	if field.Anonymous {
		return u.processAnonymousField(field, value, m, fullName)
	}

	return u.processNamedField(field, value, m, fullName)
}

func (u *Unmarshaler) processFieldNotFromString(fieldType reflect.Type, value reflect.Value,
	vp valueWithParent, opts *fieldOptionsWithContext, fullName string) error {
	derefedFieldType := Deref(fieldType)
	typeKind := derefedFieldType.Kind()
	valueKind := reflect.TypeOf(vp.value).Kind()
	mapValue := vp.value

	switch {
	case valueKind == reflect.Map && typeKind == reflect.Struct:
		mv, ok := mapValue.(map[string]interface{})
		if !ok {
			return errTypeMismatch
		}

		return u.processFieldStruct(fieldType, value, &simpleValuer{
			current: mapValuer(mv),
			parent:  vp.parent,
		}, fullName)
	case valueKind == reflect.Map && typeKind == reflect.Map:
		return u.fillMap(fieldType, value, mapValue)
	case valueKind == reflect.String && typeKind == reflect.Map:
		return u.fillMapFromString(value, mapValue)
	case valueKind == reflect.String && typeKind == reflect.Slice:
		return u.fillSliceFromString(fieldType, value, mapValue)
	case valueKind == reflect.String && derefedFieldType == durationType:
		return fillDurationValue(fieldType.Kind(), value, mapValue.(string))
	default:
		return u.processFieldPrimitive(fieldType, value, mapValue, opts, fullName)
	}
}

func (u *Unmarshaler) processFieldPrimitive(fieldType reflect.Type, value reflect.Value,
	mapValue interface{}, opts *fieldOptionsWithContext, fullName string) error {
	typeKind := Deref(fieldType).Kind()
	valueKind := reflect.TypeOf(mapValue).Kind()

	switch {
	case typeKind == reflect.Slice && valueKind == reflect.Slice:
		return u.fillSlice(fieldType, value, mapValue)
	case typeKind == reflect.Map && valueKind == reflect.Map:
		return u.fillMap(fieldType, value, mapValue)
	default:
		switch v := mapValue.(type) {
		case json.Number:
			return u.processFieldPrimitiveWithJSONNumber(fieldType, value, v, opts, fullName)
		default:
			if typeKind == valueKind {
				if err := validateValueInOptions(mapValue, opts.options()); err != nil {
					return err
				}

				return fillWithSameType(fieldType, value, mapValue, opts)
			}
		}
	}

	return newTypeMismatchError(fullName)
}

func (u *Unmarshaler) processFieldPrimitiveWithJSONNumber(fieldType reflect.Type, value reflect.Value,
	v json.Number, opts *fieldOptionsWithContext, fullName string) error {
	fieldKind := fieldType.Kind()
	typeKind := Deref(fieldType).Kind()

	if err := validateJsonNumberRange(v, opts); err != nil {
		return err
	}

	if err := validateValueInOptions(v, opts.options()); err != nil {
		return err
	}

	if fieldKind == reflect.Ptr {
		value = value.Elem()
	}

	switch typeKind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		iValue, err := v.Int64()
		if err != nil {
			return err
		}

		value.SetInt(iValue)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		iValue, err := v.Int64()
		if err != nil {
			return err
		}

		if iValue < 0 {
			return fmt.Errorf("unmarshal %q with bad value %q", fullName, v.String())
		}

		value.SetUint(uint64(iValue))
	case reflect.Float32, reflect.Float64:
		fValue, err := v.Float64()
		if err != nil {
			return err
		}

		value.SetFloat(fValue)
	default:
		return newTypeMismatchError(fullName)
	}

	return nil
}

func (u *Unmarshaler) processFieldStruct(fieldType reflect.Type, value reflect.Value,
	m valuerWithParent, fullName string) error {
	if fieldType.Kind() == reflect.Ptr {
		baseType := Deref(fieldType)
		target := reflect.New(baseType).Elem()
		if err := u.unmarshalWithFullName(m, target.Addr().Interface(), fullName); err != nil {
			return err
		}

		value.Set(target.Addr())
	} else if err := u.unmarshalWithFullName(m, value.Addr().Interface(), fullName); err != nil {
		return err
	}

	return nil
}

func (u *Unmarshaler) processFieldTextUnmarshaler(fieldType reflect.Type, value reflect.Value,
	mapValue interface{}) (bool, error) {
	var tval encoding.TextUnmarshaler
	var ok bool

	if fieldType.Kind() == reflect.Ptr {
		tval, ok = value.Interface().(encoding.TextUnmarshaler)
	} else {
		tval, ok = value.Addr().Interface().(encoding.TextUnmarshaler)
	}
	if ok {
		switch mv := mapValue.(type) {
		case string:
			return true, tval.UnmarshalText([]byte(mv))
		case []byte:
			return true, tval.UnmarshalText(mv)
		}
	}

	return false, nil
}

func (u *Unmarshaler) processFieldWithEnvValue(fieldType reflect.Type, value reflect.Value,
	envVal string, opts *fieldOptionsWithContext, fullName string) error {
	if err := validateValueInOptions(envVal, opts.options()); err != nil {
		return err
	}

	fieldKind := fieldType.Kind()
	switch fieldKind {
	case reflect.Bool:
		val, err := strconv.ParseBool(envVal)
		if err != nil {
			return fmt.Errorf("unmarshal field %q with environment variable, %w", fullName, err)
		}

		value.SetBool(val)
		return nil
	case durationType.Kind():
		if err := fillDurationValue(fieldKind, value, envVal); err != nil {
			return fmt.Errorf("unmarshal field %q with environment variable, %w", fullName, err)
		}

		return nil
	case reflect.String:
		value.SetString(envVal)
		return nil
	default:
		return u.processFieldPrimitiveWithJSONNumber(fieldType, value, json.Number(envVal), opts, fullName)
	}
}

func (u *Unmarshaler) processNamedField(field reflect.StructField, value reflect.Value,
	m valuerWithParent, fullName string) error {
	key, opts, err := u.parseOptionsWithContext(field, m, fullName)
	if err != nil {
		return err
	}

	fullName = join(fullName, key)
	if opts != nil && len(opts.EnvVar) > 0 {
		envVal := proc.Env(opts.EnvVar)
		if len(envVal) > 0 {
			return u.processFieldWithEnvValue(field.Type, value, envVal, opts, fullName)
		}
	}

	canonicalKey := key
	if u.opts.canonicalKey != nil {
		canonicalKey = u.opts.canonicalKey(key)
	}

	valuer := createValuer(m, opts)
	mapValue, hasValue := getValue(valuer, canonicalKey)
	if !hasValue {
		return u.processNamedFieldWithoutValue(field.Type, value, opts, fullName)
	}

	return u.processNamedFieldWithValue(field.Type, value, valueWithParent{
		value:  mapValue,
		parent: valuer,
	}, key, opts, fullName)
}

func (u *Unmarshaler) processNamedFieldWithValue(fieldType reflect.Type, value reflect.Value,
	vp valueWithParent, key string, opts *fieldOptionsWithContext, fullName string) error {
	mapValue := vp.value
	if mapValue == nil {
		if opts.optional() {
			return nil
		}

		return fmt.Errorf("field %s mustn't be nil", key)
	}

	if !value.CanSet() {
		return fmt.Errorf("field %s is not settable", key)
	}

	maybeNewValue(fieldType, value)

	if yes, err := u.processFieldTextUnmarshaler(fieldType, value, mapValue); yes {
		return err
	}

	fieldKind := Deref(fieldType).Kind()
	switch fieldKind {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.Struct:
		return u.processFieldNotFromString(fieldType, value, vp, opts, fullName)
	default:
		if u.opts.fromString || opts.fromString() {
			valueKind := reflect.TypeOf(mapValue).Kind()
			if valueKind != reflect.String {
				return fmt.Errorf("error: the value in map is not string, but %s", valueKind)
			}

			options := opts.options()
			if len(options) > 0 {
				if !stringx.Contains(options, mapValue.(string)) {
					return fmt.Errorf(`error: value "%s" for field "%s" is not defined in options "%v"`,
						mapValue, key, options)
				}
			}

			return fillPrimitive(fieldType, value, mapValue, opts, fullName)
		}

		return u.processFieldNotFromString(fieldType, value, vp, opts, fullName)
	}
}

func (u *Unmarshaler) processNamedFieldWithoutValue(fieldType reflect.Type, value reflect.Value,
	opts *fieldOptionsWithContext, fullName string) error {
	derefedType := Deref(fieldType)
	fieldKind := derefedType.Kind()
	if defaultValue, ok := opts.getDefault(); ok {
		if fieldType.Kind() == reflect.Ptr {
			maybeNewValue(fieldType, value)
			value = value.Elem()
		}
		if derefedType == durationType {
			return fillDurationValue(fieldKind, value, defaultValue)
		}

		switch fieldKind {
		case reflect.Array, reflect.Slice:
			return u.fillSliceWithDefault(derefedType, value, defaultValue)
		default:
			return setValue(fieldKind, value, defaultValue)
		}
	}

	switch fieldKind {
	case reflect.Array, reflect.Map, reflect.Slice:
		if !opts.optional() {
			return u.processFieldNotFromString(fieldType, value, valueWithParent{
				value: emptyMap,
			}, opts, fullName)
		}
	case reflect.Struct:
		if !opts.optional() {
			required, err := structValueRequired(u.key, derefedType)
			if err != nil {
				return err
			}

			if required {
				return fmt.Errorf("%q is not set", fullName)
			}

			return u.processFieldNotFromString(fieldType, value, valueWithParent{
				value: emptyMap,
			}, opts, fullName)
		}
	default:
		if !opts.optional() {
			return newInitError(fullName)
		}
	}

	return nil
}

func (u *Unmarshaler) unmarshalWithFullName(m valuerWithParent, v interface{}, fullName string) error {
	rv := reflect.ValueOf(v)
	if err := ValidatePtr(&rv); err != nil {
		return err
	}

	rte := reflect.TypeOf(v).Elem()
	if rte.Kind() != reflect.Struct {
		return errValueNotStruct
	}

	rve := rv.Elem()
	numFields := rte.NumField()
	for i := 0; i < numFields; i++ {
		if err := u.processField(rte.Field(i), rve.Field(i), m, fullName); err != nil {
			return err
		}
	}

	return nil
}

// WithStringValues customizes an Unmarshaler with number values from strings.
func WithStringValues() UnmarshalOption {
	return func(opt *unmarshalOptions) {
		opt.fromString = true
	}
}

// WithCanonicalKeyFunc customizes an Unmarshaler with Canonical Key func
func WithCanonicalKeyFunc(f func(string) string) UnmarshalOption {
	return func(opt *unmarshalOptions) {
		opt.canonicalKey = f
	}
}

func createValuer(v valuerWithParent, opts *fieldOptionsWithContext) valuerWithParent {
	if opts.inherit() {
		return recursiveValuer{
			current: v,
			parent:  v.Parent(),
		}
	}

	return simpleValuer{
		current: v,
		parent:  v.Parent(),
	}
}

func fillDurationValue(fieldKind reflect.Kind, value reflect.Value, dur string) error {
	d, err := time.ParseDuration(dur)
	if err != nil {
		return err
	}

	if fieldKind == reflect.Ptr {
		value.Elem().Set(reflect.ValueOf(d))
	} else {
		value.Set(reflect.ValueOf(d))
	}

	return nil
}

func fillPrimitive(fieldType reflect.Type, value reflect.Value, mapValue interface{},
	opts *fieldOptionsWithContext, fullName string) error {
	if !value.CanSet() {
		return errValueNotSettable
	}

	baseType := Deref(fieldType)
	if fieldType.Kind() == reflect.Ptr {
		target := reflect.New(baseType).Elem()
		switch mapValue.(type) {
		case string, json.Number:
			value.Set(target.Addr())
			value = target
		}
	}

	switch v := mapValue.(type) {
	case string:
		return validateAndSetValue(baseType.Kind(), value, v, opts)
	case json.Number:
		if err := validateJsonNumberRange(v, opts); err != nil {
			return err
		}
		return setValue(baseType.Kind(), value, v.String())
	default:
		return newTypeMismatchError(fullName)
	}
}

func fillWithSameType(fieldType reflect.Type, value reflect.Value, mapValue interface{},
	opts *fieldOptionsWithContext) error {
	if !value.CanSet() {
		return errValueNotSettable
	}

	if err := validateValueRange(mapValue, opts); err != nil {
		return err
	}

	if fieldType.Kind() == reflect.Ptr {
		baseType := Deref(fieldType)
		target := reflect.New(baseType).Elem()
		setSameKindValue(baseType, target, mapValue)
		value.Set(target.Addr())
	} else {
		setSameKindValue(fieldType, value, mapValue)
	}

	return nil
}

// getValue gets the value for the specific key, the key can be in the format of parentKey.childKey
func getValue(m valuerWithParent, key string) (interface{}, bool) {
	keys := readKeys(key)
	return getValueWithChainedKeys(m, keys)
}

func getValueWithChainedKeys(m valuerWithParent, keys []string) (interface{}, bool) {
	switch len(keys) {
	case 0:
		return nil, false
	case 1:
		v, ok := m.Value(keys[0])
		return v, ok
	default:
		if v, ok := m.Value(keys[0]); ok {
			if nextm, ok := v.(map[string]interface{}); ok {
				return getValueWithChainedKeys(recursiveValuer{
					current: mapValuer(nextm),
					parent:  m,
				}, keys[1:])
			}
		}

		return nil, false
	}
}

func join(elem ...string) string {
	var builder strings.Builder

	var fillSep bool
	for _, e := range elem {
		if len(e) == 0 {
			continue
		}

		if fillSep {
			builder.WriteByte(delimiter)
		} else {
			fillSep = true
		}

		builder.WriteString(e)
	}

	return builder.String()
}

func newInitError(name string) error {
	return fmt.Errorf("field %s is not set", name)
}

func newTypeMismatchError(name string) error {
	return fmt.Errorf("error: type mismatch for field %s", name)
}

func readKeys(key string) []string {
	cacheKeysLock.Lock()
	keys, ok := cacheKeys[key]
	cacheKeysLock.Unlock()
	if ok {
		return keys
	}

	keys = strings.FieldsFunc(key, func(c rune) bool {
		return c == delimiter
	})
	cacheKeysLock.Lock()
	cacheKeys[key] = keys
	cacheKeysLock.Unlock()

	return keys
}

func setSameKindValue(targetType reflect.Type, target reflect.Value, value interface{}) {
	if reflect.ValueOf(value).Type().AssignableTo(targetType) {
		target.Set(reflect.ValueOf(value))
	} else {
		target.Set(reflect.ValueOf(value).Convert(targetType))
	}
}
