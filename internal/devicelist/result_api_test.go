package devicelist

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// Embedding would make Admitted ambiguous if it were added back to either
// Result method set, so both assignments are compile-time absence guards.
type admittedNameGuard struct{}

func (admittedNameGuard) Admitted() {}

type resultMethodGuard struct {
	Result
	admittedNameGuard
}

type resultPointerMethodGuard struct {
	*Result
	admittedNameGuard
}

var (
	_ interface{ Admitted() }              = resultMethodGuard{}
	_ interface{ Admitted() }              = resultPointerMethodGuard{}
	_ interface{ ObservedInstruct() bool } = Result{}
)

// Claim: Result and *Result expose only the observation-named accessor, never an Admitted method.
func TestResultObservationAPI(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Result{}), reflect.TypeOf(&Result{})} {
		if _, ok := typ.MethodByName("ObservedInstruct"); !ok {
			t.Fatal("observation accessor control missing")
		}
		if _, ok := typ.MethodByName("Admitted"); ok || typ.NumMethod() != 1 {
			t.Fatal("unexpected result method set:", typ)
		}
	}
}

func parsedSource(t testing.TB, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// Claim: the package, Snapshot, ReadSnapshot and stat-check comments disclose both the Darwin ACL gap and Linux ENOTSUP limit.
func TestSnapshotACLDisclosures(t *testing.T) {
	docs := map[string]string{"package": parsedSource(t, "doc.go").Doc.Text()}
	for _, path := range []string{"snapshot.go", "snapshot_unix.go"} {
		for _, decl := range parsedSource(t, path).Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "ReadSnapshot" || d.Name.Name == "snapshotStatSafe" {
					docs[d.Name.Name] = d.Doc.Text()
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if typ, ok := spec.(*ast.TypeSpec); ok && typ.Name.Name == "Snapshot" {
						docs[typ.Name.Name] = d.Doc.Text()
					}
				}
			}
		}
	}
	if len(docs) != 4 {
		t.Fatal("missing API documentation control:", docs)
	}
	for name, doc := range docs {
		for _, term := range []string{"Darwin", "not check ACLs", "Linux", "ENOTSUP", "not checked"} {
			if !strings.Contains(doc, term) {
				t.Fatalf("%s omits ACL limitation %q", name, term)
			}
		}
	}
}

// Claim: the three parser-only snapshot shapes are private while Snapshot remains public, and operational-error docs retain ObserveOnly.
func TestPrivateSnapshotShapes(t *testing.T) {
	found := map[string]bool{}
	for _, decl := range parsedSource(t, "snapshot.go").Decls {
		if d, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range d.Specs {
				if typ, ok := spec.(*ast.TypeSpec); ok {
					switch strings.ToLower(typ.Name.Name) {
					case "snapshotdata", "namerecord", "sealrecord":
						if typ.Name.IsExported() {
							t.Fatal("obsolete parser shape exported:", typ.Name.Name)
						}
					case "snapshotdatavalue", "snapshotnamerecord", "snapshotsealrecord":
						if typ.Name.IsExported() {
							t.Fatal("parser shape exported:", typ.Name.Name)
						}
						found[typ.Name.Name] = true
					case "snapshot":
						if !typ.Name.IsExported() {
							t.Fatal("public view control missing")
						}
						found[typ.Name.Name] = true
					}
				}
			}
		}
	}
	if len(found) != 4 {
		t.Fatal("snapshot shape count:", found)
	}
	file := parsedSource(t, "codes.go")
	for _, group := range file.Comments {
		text := group.Text()
		if strings.Contains(text, "Operational failures.") {
			if !strings.Contains(text, "Result{ObserveOnly: true}") || strings.Contains(text, "zero Result") {
				t.Fatal("operational-error comment loses observe-only marking")
			}
			return
		}
	}
	t.Fatal("operational-error comment missing")
}

// Claim: journal storage has only contentless observe closures, no enforcement state, and a closure-producing result remains observe-only.
func TestJournalObservationState(t *testing.T) {
	allowed := map[string]bool{"hasHead": true, "head": true, "closedKeys": true, "closedSeals": true, "seenUnspent": true, "replay": true, "full": true}
	typ := reflect.TypeOf(ownerJournal{})
	if typ.NumField() != len(allowed) {
		t.Fatal("unexpected journal state field count")
	}
	for i := 0; i < typ.NumField(); i++ {
		if !allowed[typ.Field(i).Name] {
			t.Fatal("unexpected journal state:", typ.Field(i).Name)
		}
	}
	keys, _ := typ.FieldByName("closedKeys")
	seals, _ := typ.FieldByName("closedSeals")
	if keys.Type != reflect.TypeOf(map[string]observeClosure{}) || seals.Type != reflect.TypeOf(map[Outpoint]observeClosure{}) || reflect.TypeOf(observeClosure{}).NumField() != 0 {
		t.Fatal("closure storage is not contentless observe-grade state")
	}
	s := newScenario(t)
	wantObservedInstruct(t, s)
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	r := wantRefused(t, s, DeviceRevoked, 8)
	if !r.ObserveOnly || !isClosed(s, keyEdE, sealE) {
		t.Fatal("observe-closure control missing or unmarked")
	}
}
