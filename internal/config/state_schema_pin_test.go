package config

import (
	htmltemplate "html/template"
	"io"
	"reflect"
	"strings"
	"testing"
	texttemplate "text/template"
	"unsafe"

	"github.com/relux-works/a2a-swarm/internal/config/internal/schemapinfixture"
)

// schemaTypePair is two types whose canonical encodings differ in exactly one
// property (kind names the reflect.Kind whose property it is), or, when same is
// set, two spellings of one identical structure that must share a pin.
type schemaTypePair struct {
	name string
	kind reflect.Kind
	a, b reflect.Type
	same bool
}

// fieldOf is an unnamed root struct with one exported field X of type t, so a
// pair built from it differs only in t.
func fieldOf(t reflect.Type) reflect.Type {
	return reflect.StructOf([]reflect.StructField{{Name: "X", Type: t}})
}

// basicKindPairs pairs every basic kind with the next one, each as the type of
// the same field.
func basicKindPairs() []schemaTypePair {
	basics := []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](),
		reflect.TypeFor[int32](), reflect.TypeFor[int64](), reflect.TypeFor[uint](), reflect.TypeFor[uint8](),
		reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[uintptr](),
		reflect.TypeFor[float32](), reflect.TypeFor[float64](), reflect.TypeFor[complex64](), reflect.TypeFor[complex128](),
		reflect.TypeFor[string](), reflect.TypeFor[unsafe.Pointer](),
	}
	var out []schemaTypePair
	for i, t := range basics {
		next := basics[(i+1)%len(basics)]
		out = append(out, schemaTypePair{"basic " + t.Kind().String() + " to " + next.Kind().String(), t.Kind(), fieldOf(t), fieldOf(next), false})
	}
	return out
}

// signaturePairs are the regression rows for schema-pin-signature-types-blind:
// a component reached only through an interface method or a func signature
// changes its package identity or named payload structure. Each pair shares
// its type name and package path and the reflect.Type.String spelling of the
// changed signature.
func signaturePairs() []schemaTypePair {
	return []schemaTypePair{
		{"method result package identity", reflect.Interface, func() reflect.Type {
			type pinFixture struct {
				Hook interface{ T() *htmltemplate.Template }
			}
			return reflect.TypeFor[pinFixture]()
		}(), func() reflect.Type {
			type pinFixture struct {
				Hook interface{ T() *texttemplate.Template }
			}
			return reflect.TypeFor[pinFixture]()
		}(), false},
		{"method result named slice element", reflect.Interface, func() reflect.Type {
			type pinList []string
			type pinFixture struct {
				Hook interface{ M() pinList }
			}
			return reflect.TypeFor[pinFixture]()
		}(), func() reflect.Type {
			type pinList []int
			type pinFixture struct {
				Hook interface{ M() pinList }
			}
			return reflect.TypeFor[pinFixture]()
		}(), false},
		{"method argument named map key", reflect.Interface, func() reflect.Type {
			type pinArgs map[string]bool
			type pinFixture struct {
				Hook interface{ M(pinArgs) }
			}
			return reflect.TypeFor[pinFixture]()
		}(), func() reflect.Type {
			type pinArgs map[int]bool
			type pinFixture struct {
				Hook interface{ M(pinArgs) }
			}
			return reflect.TypeFor[pinFixture]()
		}(), false},
		{"func result named slice element", reflect.Func, func() reflect.Type {
			type pinList []string
			type pinFixture struct {
				Next func() pinList
			}
			return reflect.TypeFor[pinFixture]()
		}(), func() reflect.Type {
			type pinList []*string
			type pinFixture struct {
				Next func() pinList
			}
			return reflect.TypeFor[pinFixture]()
		}(), false},
		{"func argument payload struct", reflect.Func, func() reflect.Type {
			type pinPayload struct{ A []string }
			type pinFixture struct {
				On func(pinPayload) error
			}
			return reflect.TypeFor[pinFixture]()
		}(), func() reflect.Type {
			type pinPayload struct{ A []*string }
			type pinFixture struct {
				On func(pinPayload) error
			}
			return reflect.TypeFor[pinFixture]()
		}(), false},
	}
}

// structurePairs are single-property differences for the composite kinds and
// type identity, followed by identical-structure rows that must match.
func structurePairs() []schemaTypePair {
	sameNameA := func() reflect.Type {
		type node struct {
			Next *node
			V    int
		}
		return reflect.TypeFor[node]()
	}()
	sameNameB := func() reflect.Type {
		type node struct {
			Next *node
			V    string
		}
		return reflect.TypeFor[node]()
	}()
	twoNodes := func(third reflect.Type) reflect.Type {
		return reflect.StructOf([]reflect.StructField{
			{Name: "A", Type: reflect.PointerTo(sameNameA)},
			{Name: "B", Type: reflect.PointerTo(sameNameB)},
			{Name: "C", Type: reflect.PointerTo(third)},
		})
	}
	return []schemaTypePair{
		{"pointer element", reflect.Pointer, reflect.TypeFor[*int](), reflect.TypeFor[*string](), false},
		{"slice element", reflect.Slice, reflect.TypeFor[[]int](), reflect.TypeFor[[]int8](), false},
		{"slice to array", reflect.Slice, reflect.TypeFor[[]int](), reflect.TypeFor[[1]int](), false},
		{"array length", reflect.Array, reflect.TypeFor[[2]int](), reflect.TypeFor[[3]int](), false},
		{"array element", reflect.Array, reflect.TypeFor[[2]int](), reflect.TypeFor[[2]uint](), false},
		{"map key", reflect.Map, reflect.TypeFor[map[string]int](), reflect.TypeFor[map[int]int](), false},
		{"map element", reflect.Map, reflect.TypeFor[map[string]int](), reflect.TypeFor[map[string]bool](), false},
		{"map key named structure", reflect.Map, func() reflect.Type {
			type pinKey struct{ A string }
			return reflect.TypeFor[map[pinKey]int]()
		}(), func() reflect.Type {
			type pinKey struct{ A int }
			return reflect.TypeFor[map[pinKey]int]()
		}(), false},
		{"chan direction both to receive", reflect.Chan, reflect.TypeFor[chan int](), reflect.TypeFor[<-chan int](), false},
		{"chan direction both to send", reflect.Chan, reflect.TypeFor[chan int](), reflect.TypeFor[chan<- int](), false},
		{"chan direction receive to send", reflect.Chan, reflect.TypeFor[<-chan int](), reflect.TypeFor[chan<- int](), false},
		{"chan element", reflect.Chan, reflect.TypeFor[chan int](), reflect.TypeFor[chan string](), false},
		{"func variadic", reflect.Func, reflect.TypeFor[func(...int)](), reflect.TypeFor[func([]int)](), false},
		{"func input type", reflect.Func, reflect.TypeFor[func(int)](), reflect.TypeFor[func(string)](), false},
		{"func output type", reflect.Func, reflect.TypeFor[func() int](), reflect.TypeFor[func() string](), false},
		{"func input to output", reflect.Func, reflect.TypeFor[func(int)](), reflect.TypeFor[func() int](), false},
		{"func input arity", reflect.Func, reflect.TypeFor[func(int)](), reflect.TypeFor[func(int, int)](), false},
		{"func output order", reflect.Func, reflect.TypeFor[func() (int, string)](), reflect.TypeFor[func() (string, int)](), false},
		{"interface method name", reflect.Interface, reflect.TypeFor[interface{ A() }](), reflect.TypeFor[interface{ B() }](), false},
		{"interface exported to unexported method", reflect.Interface, reflect.TypeFor[interface{ M() }](), reflect.TypeFor[interface{ m() }](), false},
		{"interface method added", reflect.Interface, reflect.TypeFor[interface{ A() }](), reflect.TypeFor[interface {
			A()
			B()
		}](), false},
		{"interface method type", reflect.Interface, reflect.TypeFor[interface{ A(int) }](), reflect.TypeFor[interface{ A(uint) }](), false},
		{"interface embedded method set", reflect.Interface, reflect.TypeFor[interface{ io.Reader }](), reflect.TypeFor[interface{ io.ReadCloser }](), false},
		{"struct field name", reflect.Struct, reflect.TypeFor[struct{ A int }](), reflect.TypeFor[struct{ B int }](), false},
		{"struct field tag", reflect.Struct, reflect.TypeFor[struct {
			A int `json:"a"`
		}](), reflect.TypeFor[struct {
			A int `json:"b"`
		}](), false},
		{"struct field type", reflect.Struct, reflect.TypeFor[struct{ A int }](), reflect.TypeFor[struct{ A int64 }](), false},
		{"struct field package path", reflect.Struct, reflect.TypeFor[struct{ A int }](), reflect.TypeFor[struct{ a int }](), false},
		{"struct field embedded", reflect.Struct, reflect.TypeFor[struct{ Notification Notification }](), reflect.TypeFor[struct{ Notification }](), false},
		{"struct field order", reflect.Struct, reflect.TypeFor[struct{ A, B int }](), reflect.TypeFor[struct{ B, A int }](), false},
		{"named type package identity", reflect.Struct, reflect.TypeFor[struct{ T *htmltemplate.Template }](), reflect.TypeFor[struct{ T *texttemplate.Template }](), false},
		{"named to unnamed field type", reflect.Slice, func() reflect.Type {
			type pinList []int
			return fieldOf(reflect.TypeFor[pinList]())
		}(), reflect.TypeFor[struct{ X []int }](), false},
		{"named kind", reflect.String, func() reflect.Type {
			type pinKind string
			return fieldOf(reflect.TypeFor[pinKind]())
		}(), func() reflect.Type {
			type pinKind int
			return fieldOf(reflect.TypeFor[pinKind]())
		}(), false},
		{"named type package path only", reflect.Int, func() reflect.Type {
			type Pos int
			return fieldOf(reflect.TypeFor[Pos]())
		}(), fieldOf(reflect.TypeFor[schemapinfixture.Pos]()), false},
		{"unexported field package path only", reflect.Struct, reflect.TypeFor[struct{ a int }](), reflect.TypeFor[schemapinfixture.Hidden](), false},
		{"unexported method package path only", reflect.Interface, reflect.TypeFor[interface{ m() }](), reflect.TypeFor[schemapinfixture.Sealed](), false},
		{"recursive type field", reflect.Struct, sameNameA, sameNameB, false},
		{"back-reference target among same-named types", reflect.Struct, twoNodes(sameNameA), twoNodes(sameNameB), false},

		{"alias spelling", reflect.Struct, func() reflect.Type {
			type pinAlias = Notification
			type pinCallback = func(pinAlias) error
			return reflect.TypeFor[struct {
				N  pinAlias
				On pinCallback
			}]()
		}(), reflect.TypeFor[struct {
			N  Notification
			On func(Notification) error
		}](), true},
		{"embedded interface spelled as methods", reflect.Interface, reflect.TypeFor[interface{ io.Reader }](), reflect.TypeFor[interface {
			Read([]byte) (int, error)
		}](), true},
		{"distinct same-named recursive types of equal structure", reflect.Struct, sameNameA, func() reflect.Type {
			type node struct {
				Next *node
				V    int
			}
			return reflect.TypeFor[node]()
		}(), true},
	}
}

// Claim: for every row, two types that differ in exactly one encoded property
// (each basic kind, pointer/slice element, array length and element, map key
// and element, chan direction and element, func variadic flag, input/output
// types, arity and order, interface method name, export, type and embedded
// method set, struct field name, tag, type, package path, embedding and
// order, named-type package identity, the package path alone of a named
// type, an unexported field or an unexported method, named versus unnamed,
// recursive field, back-reference target) have different pins and the changed one fails the
// reviewed pin; two spellings of one structure (aliases, embedded interface
// versus its methods, distinct same-named recursive types of equal structure)
// share a pin. Every reflect.Kind except Invalid has a differing row, and the
// current State matches stateCloneSchemaPin. Limit: rows are local fixture
// types, not edits of State, and the table is examples, not an enumeration of
// all type pairs.
func TestStateSchemaPinTypeProperties(t *testing.T) {
	if !stateSchemaReviewed(reflect.TypeFor[State](), stateCloneSchemaPin) {
		t.Errorf("current State does not match the pin (%s)", stateSchemaSignature())
	}
	rows := append(append(basicKindPairs(), signaturePairs()...), structurePairs()...)
	covered := map[reflect.Kind]bool{}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			pin := stateSchemaSignatureOf(tc.a)
			if stateSchemaSignatureOf(tc.a) != pin {
				t.Fatal("signature is not deterministic")
			}
			if !stateSchemaReviewed(tc.a, pin) {
				t.Fatal("type does not match its own pin")
			}
			if tc.same {
				if strings.HasPrefix(tc.name, "distinct") && tc.a == tc.b {
					t.Fatalf("fixture premise: %s is one type", tc.a)
				}
				if !stateSchemaReviewed(tc.b, pin) {
					t.Fatalf("identical structure has a different pin: %s / %s", tc.a, tc.b)
				}
				return
			}
			if tc.a == tc.b {
				t.Fatalf("fixture premise: the pair is one type %s", tc.a)
			}
			if stateSchemaReviewed(tc.b, pin) {
				t.Fatalf("changed type keeps the reviewed pin: %s / %s", tc.a, tc.b)
			}
		})
		if !tc.same {
			covered[tc.kind] = true
		}
	}
	for k := reflect.Bool; k <= reflect.UnsafePointer; k++ {
		if !covered[k] {
			t.Errorf("no differing row for kind %s", k)
		}
	}
}

// Claim (regression witness for schema-pin-signature-types-blind): a method
// result whose package identity changes, or a named method/func argument or
// result whose payload structure changes, while the signature's
// reflect.Type.String spelling and the fixture's name stay equal, changes the
// pin; the public loader and writer then decode ordinarily (no admission, hit
// or writer clone), while the reviewed neighbour matches its pin and is
// reused.
func TestStateCloneSchemaPinDetectsSignatureTypes(t *testing.T) {
	for _, tc := range signaturePairs() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.a.Name() != tc.b.Name() || tc.a.PkgPath() != tc.b.PkgPath() || tc.a.NumField() != 1 || tc.b.NumField() != 1 {
				t.Fatalf("fixture premise: %s / %s", tc.a, tc.b)
			}
			fa, fb := tc.a.Field(0), tc.b.Field(0)
			if fa.Name != fb.Name || fa.Tag != fb.Tag || fa.Type.String() != fb.Type.String() {
				t.Fatalf("fixture premise: signature strings differ: %s / %s", fa.Type, fb.Type)
			}
			pin := stateSchemaSignatureOf(tc.a)
			if stateSchemaSignatureOf(tc.b) == pin {
				t.Fatal("signature change keeps the reviewed pin")
			}
			if !schemaGateReuse(t, tc.a, pin) {
				t.Fatal("reviewed schema disabled reuse")
			}
			if schemaGateReuse(t, tc.b, pin) {
				t.Fatal("changed signature reused parsed data")
			}
		})
	}
}
