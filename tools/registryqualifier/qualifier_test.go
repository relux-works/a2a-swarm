package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Entirely synthetic; these names and values have no provider provenance.
const synthetic = "function helper(input){return{payload:input,tag:\"record\"}}function alternate(input){return{payload:input,tag:\"different\"}}class Record{constructor(input){this.value=input}static from(input){return{value:input}}}function create(input){const temp=\x60raw } \\` ${input/2} ${/}/}\x60;return{pid:input,sessionId:\"synthetic-session\",status:\"idle\",VERSION:\"1\",BUILD_TIME:\"day\",GIT_SHA:\"abc\",name:helper(input),record:new Record(input)}}function readRecord(input){return helper(input)}function patchRecord(input,delta){return{...input,status:delta,note:\"late\",updatedAt:2}}"

var baseAnchors = anchors{"create", "readRecord", "patchRecord"}

func parsed(t *testing.T, data string) *program {
	t.Helper()
	p, err := parse(context.Background(), []byte(data))
	if err != nil {
		t.Fatalf("synthetic parse refused: %s", err)
	}
	return p
}
func compareSynthetic(t *testing.T, source string) ([]functionComparison, string) {
	t.Helper()
	return comparePrograms(parsed(t, synthetic), parsed(t, source), baseAnchors, baseAnchors)
}
func evaluated(t *testing.T, source string) report {
	t.Helper()
	return evaluate(context.Background(), []byte(synthetic), []byte(source), baseAnchors, baseAnchors, 60)
}
func requireControls(t *testing.T, r report) {
	t.Helper()
	if len(r.Controls) != 3 {
		t.Fatalf("missing controls: %+v", r.Controls)
	}
	for _, c := range r.Controls {
		if c.Status != "PASS" || !c.Planted || !c.Detected || c.MutantSHA256 == r.BaseSHA256 || c.Observation != different {
			t.Fatalf("control did not detect its actual mutation: %+v", c)
		}
	}
}
func TestEqualAfterRenaming(t *testing.T) {
	// Replace complete binding names before the shared class substring.
	other := strings.NewReplacer("readRecord", "g", "patchRecord", "u", "helper", "h", "alternate", "j", "Record", "K", "create", "f", "input", "v", "delta", "d", "temp", "q").Replace(synthetic)
	other = strings.NewReplacer("VERSION:\"1\"", "VERSION:\"99\"", "BUILD_TIME:\"day\"", "BUILD_TIME:\"night\"", "GIT_SHA:\"abc\"", "GIT_SHA:\"xyz\"").Replace(other)
	r := evaluate(context.Background(), []byte(synthetic), []byte(other), baseAnchors, anchors{"f", "g", "u"}, 60)
	if r.Status != equal {
		t.Fatalf("rename must compare equal: %+v", r)
	}
	requireControls(t, r)
}

func TestLiteralRegression(t *testing.T) {
	got, status := compareSynthetic(t, strings.Replace(synthetic, "tag:\"record\"", "tag:\"changed\"", 1))
	if status != different || got[0].Callees[0].Status != different {
		t.Fatalf("callee literal change must differ: %+v", got)
	}
}
func TestPropertyNeighbor(t *testing.T) {
	got, status := compareSynthetic(t, strings.Replace(synthetic, "pid:input", "processId:input", 1))
	if status != different || got[0].Body != different || got[1].Status != equal || got[2].Status != equal {
		t.Fatalf("property change and neighbors: %+v", got)
	}
}
func TestCompleteFunctionsAndLiteralSpellings(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		role           int
	}{
		{"constructor-property", "pid:input", "processId:input", 0},
		{"constructor-literal", "\"synthetic-session\"", "\"new-synthetic-session\"", 0},
		{"last-update-literal", "updatedAt:2", "updatedAt:3", 2},
		{"last-update-property", "updatedAt:2", "changedAt:2", 2},
		{"escaped-string", "note:\"late\"", "note:\"\\x6cate\"", 2},
		{"numeric-spelling", "updatedAt:2", "updatedAt:0x2", 2},
		{"template-raw", "raw }", "new }", 0},
		{"template-expression", "input/2", "input/3", 0},
		{"regexp", "/}/", "/x/", 0},
		{"regexp-flags", "/}/", "/}/g", 0},
		{"nonmetadata", "status:\"idle\"", "status:\"busy\"", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(synthetic, tc.old, tc.new, 1)
			if source == synthetic {
				t.Fatal("test did not mutate")
			}
			r := evaluated(t, source)
			if r.Status != different || r.Functions[tc.role].Body != different {
				t.Fatalf("change not detected: %+v", r)
			}
			requireControls(t, r)
		})
	}
}
func TestOneLevelFunctionAndClassCallees(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		edge           int
	}{
		{"swap", "name:helper(input)", "name:alternate(input)", 0},
		{"function-body", "tag:\"record\"", "tag:\"changed\"", 0},
		{"class-body", "this.value=input", "this.other=input", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := evaluated(t, strings.Replace(synthetic, tc.old, tc.new, 1))
			if r.Status != different || r.Functions[0].Callees[tc.edge].Status != different {
				t.Fatalf("callee change not detected: %+v", r)
			}
			requireControls(t, r)
		})
	}
}
func TestRefusedAnchorsAndCallees(t *testing.T) {
	for _, tc := range []struct{ name, source, reason string }{
		{"missing", strings.Replace(synthetic, "function create(", "function absent(", 1), "candidate_missing_anchor"},
		{"duplicate-anchor", synthetic + ";function create(input){return input}", "candidate_ambiguous_anchor"},
		{"unresolved", strings.Replace(synthetic, "name:helper(input)", "name:missing(input)", 1), "candidate_unresolved_or_ambiguous_callee"},
		{"duplicate-callee", synthetic + ";function helper(input){return input}", "candidate_unresolved_or_ambiguous_callee"},
		{"parameter-callee", strings.Replace(synthetic, "name:helper(input)", "name:input(input)", 1), "candidate_nonfunction_callee"},
		{"member-callee", strings.Replace(synthetic, "name:helper(input)", "name:input.read(input)", 1), "candidate_nonfunction_callee"},
		{"computed-callee", strings.Replace(synthetic, "name:helper(input)", "name:helper[\"read\"](input)", 1), "candidate_dynamic_callee"},
		{"callee-of-callee-unresolved", strings.Replace(synthetic, "payload:input,tag:\"record\"", "payload:missing(input),tag:\"record\"", 1), "candidate_unresolved_or_ambiguous_callee"},
		{"mutated-callee", synthetic + ";helper=alternate;", "candidate_mutable_callee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := evaluated(t, tc.source)
			if r.Status == equal {
				t.Fatal("refusal returned equal")
			}
			got := r.Functions[0]
			reason := got.Reason
			for _, c := range got.Callees {
				if c.Reason != "" {
					reason = c.Reason
					break
				}
			}
			if reason != tc.reason || got.Status != notCompared {
				t.Fatalf("want %s, got %+v", tc.reason, got)
			}
			requireControls(t, r)
		})
	}
}
func TestAlphaRenamingRetainsBindingRelationships(t *testing.T) {
	base := "function f(a,b){let c=a;return {one:c,two:b}}function r(a){return a}function u(a){return a}"
	p := parsed(t, base)
	q := parsed(t, strings.Replace(base, "two:b", "two:a", 1))
	got, s := comparePrograms(p, q, anchors{"f", "r", "u"}, anchors{"f", "r", "u"})
	if s != different || got[0].Body != different {
		t.Fatalf("alias relationship erased: %+v", got)
	}
	base = "function f(a){let b=a;{let a=2;b=a}return {one:b}}function r(a){return a}function u(a){return a}"
	other := "function f(x){let y=x;{let z=2;y=z}return {one:y}}function r(x){return x}function u(x){return x}"
	_, s = comparePrograms(parsed(t, base), parsed(t, other), anchors{"f", "r", "u"}, anchors{"f", "r", "u"})
	if s != equal {
		t.Fatal("lexical shadow rename must match")
	}
}
func TestLexicalCalleeResolution(t *testing.T) {
	base := strings.Replace(synthetic, "function create(input){", "function create(input){function helper(x){return {nested:x}}", 1)
	other := strings.Replace(base, "tag:\"record\"", "tag:\"changed\"", 1)
	got, s := comparePrograms(parsed(t, base), parsed(t, other), baseAnchors, baseAnchors)
	if s != different || got[0].Status != equal || got[1].Status != different {
		t.Fatalf("lexical target resolution failed: %+v", got)
	}
}
func TestStaticClassAndArrow(t *testing.T) {
	base := strings.Replace(synthetic, "function helper(input){return{payload:input,tag:\"record\"}}", "const helper=(input)=>({payload:input,tag:\"record\"});", 1)
	base = strings.Replace(base, "return helper(input)}function patchRecord", "return Record.from(input)}function patchRecord", 1)
	r := evaluate(context.Background(), []byte(base), []byte(base), baseAnchors, baseAnchors, 60)
	if r.Status != equal {
		t.Fatalf("supported arrow and class: %+v", r)
	}
	requireControls(t, r)
}
func TestMetadataMaskIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		old, new string
		want     string
	}{
		{"VERSION:\"1\"", "VERSION:123", equal},
		{"VERSION:\"1\"", "version:\"1\"", different},
		{"VERSION:\"1\"", "OTHER:\"1\"", different},
		{"VERSION:\"1\"", "VERSION:input+1", different},
		{"BUILD_TIME:\"day\"", "BUILD_TIME_EXTRA:\"day\"", different},
		{"GIT_SHA:\"abc\"", "GIT_SHA:\x60${input+1}\x60", different},
	} {
		t.Run(tc.new, func(t *testing.T) {
			_, s := compareSynthetic(t, strings.Replace(synthetic, tc.old, tc.new, 1))
			if s != tc.want {
				t.Fatalf("want %s got %s", tc.want, s)
			}
		})
	}
}
func TestMalformedAndUnsupportedFailClosed(t *testing.T) {
	for _, s := range []string{
		"function f(){return {x:};}", "function f(){return (1+);}", "function f(){return 'no}", "function f(){/* no}", "function f(){return /no}", "function f(){return /(/;}", "function f(){return /x/gg;}", "function f(){return `no}", "function f(){return `x${1+}`}", "function f(){return `x${{a:1}`}",
		"function f(){return x?.a}", "function* f(){yield 1}", "class C extends B{}", "import x from 'x';", "const f=async(a)=>a;", "const f=({a})=>a;", "function f(){for(a in b){}}", "function f(){with(x){}}", "function f(){throw\n1}", "return 1;", "function f(){break;}", "let x=0x;", "let x=1e;", "let x='\\u{}';", "let x='\\8';", "function f(){return /x/v;}",
		"function f(){return /a{2,1}/;}", "function f(){return /(?=a)/;}", "function f(){return /x/u;}", "function f(){return a.\"x\";}", "function f(){return a.2;}", "function f(){return (a,);}", "function f(){let a=1;let a=2;return a}", "function f(a,a){return a}", "function f(){return await a}", "function f(){return -2**2}", "function f(){return a??b||c}", "function f(){return a&&b??c}", "function f(){return delete a}", "function f(){return {get x(a){return a}}}", "class C{async constructor(){}}",
	} {
		t.Run(s, func(t *testing.T) {
			if _, err := parse(context.Background(), []byte(s)); err == nil {
				t.Fatal("malformed/unsupported input accepted")
			}
		})
	}
}
func TestSupportedLexicalAndStatementForms(t *testing.T) {
	for _, s := range []string{
		"function f(a){if(a)/}/.test(a);else a=a/2;return a}",
		"function f(a){let {x:y,z}=a;let [p,,q=2,...rest]=y;return {z,y,p,q,rest}}",
		"function f(a){for(let i=0;i<2;i++){a+=i}while(a){a--}do{a++}while(a<0);return a}",
		"function f(a){for(const x of a){if(x)continue;break}return a}",
		"function f(a){try{throw a}catch(e){return e}finally{a=2}}",
		"function f(a){switch(a){case 1:a=2;break;default:a=3}return a}",
		"function f(a){return {value(){return a},async read(){return a},['key']:a}}",
		"function f(a){var x;{var y=a}return {x,y}}",
		"async function f(a){return await a}",
		"const f=function named(a){return a};const g=a=>a/2;",
		"function f(a){return `outer ${`inner ${/[}]/g}`} tail`}",
	} {
		t.Run(s, func(t *testing.T) { parsed(t, s) })
	}
}
func TestControlsCannotRunOrBlind(t *testing.T) {
	base := "function f(a){return a}function r(a){return a}function u(a){return a}"
	r := evaluate(context.Background(), []byte(base), []byte(base), anchors{"f", "r", "u"}, anchors{"f", "r", "u"}, 60)
	if r.Status != "CONTROL_FAILED" {
		t.Fatalf("unavailable controls must fail: %+v", r)
	}
	for _, c := range r.Controls {
		if c.Status != "FAIL" || c.Planted || c.Detected {
			t.Fatal("unavailable control claimed success")
		}
	}
}
func TestBoundsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := evaluate(ctx, []byte(synthetic), []byte(synthetic), baseAnchors, baseAnchors, 1)
	if r.Status == equal || r.Reason != "base_deadline" {
		t.Fatalf("cancel must refuse: %+v", r)
	}
	deep := "function f(){return " + strings.Repeat("(", maxDepth+1) + "1" + strings.Repeat(")", maxDepth+1) + "}"
	if _, err := parse(context.Background(), []byte(deep)); err == nil || err.Error() != "depth_limit" {
		t.Fatalf("depth not bounded: %v", err)
	}
	if err := validateInput(context.Background(), bytes.Repeat([]byte{' '}, maxBytes+1)); err == nil {
		t.Fatal("size limit not enforced")
	}
	for _, bad := range [][]byte{nil, {0}, {0xff}, []byte("let x=1\u2028x=2")} {
		if err := validateInput(context.Background(), bad); err == nil {
			t.Fatal("invalid encoding/size accepted")
		}
	}
}

func cliArgs(base, candidate string) []string {
	return []string{"-base", base, "-candidate", candidate, "-base-constructor", "create", "-candidate-constructor", "create", "-base-reader", "readRecord", "-candidate-reader", "readRecord", "-base-update", "patchRecord", "-candidate-update", "patchRecord"}
}
func TestCommandReportsAndExits(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "base.js"), filepath.Join(dir, "candidate.js")
	if err := os.WriteFile(a, []byte(synthetic), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, data string
		exit       int
	}{
		{"equal", synthetic, 0}, {"different", strings.Replace(synthetic, "updatedAt:2", "updatedAt:3", 1), 1}, {"missing", strings.Replace(synthetic, "function create(", "function missing(", 1), 1}, {"malformed", "function create(){return '\";}", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(b, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			exit := run(cliArgs(a, b), &out)
			if exit != tc.exit {
				t.Fatalf("exit %d: %s", exit, out.String())
			}
			var r report
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), dir) || strings.Contains(out.String(), "synthetic-session") || strings.Contains(out.String(), "raw }") {
				t.Fatal("report leaked input path or values")
			}
			if len(r.Controls) != 3 {
				t.Fatal("missing control results")
			}
		})
	}
	for _, args := range [][]string{nil, {"-base", "/PRIVATE_MISSING_PATH"}, {"-unknown", "PRIVATE_VALUE"}, append(cliArgs(a, b), "-seconds", "91"), append(cliArgs(a, b), "-base-reader", "not.valid")} {
		var out bytes.Buffer
		if run(args, &out) != 2 || strings.Contains(out.String(), "PRIVATE") {
			t.Fatalf("argument refusal leak: %s", out.String())
		}
	}
}
func TestExplicitFileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.js")
	if err := os.WriteFile(path, []byte(synthetic), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{dir, link, filepath.Join(dir, "missing")} {
		if _, reason := readBundle(bad); reason == "" {
			t.Fatal("nonregular/missing file accepted")
		}
	}
	data, reason := readBundle(path)
	if reason != "" || !bytes.Equal(data, []byte(synthetic)) {
		t.Fatal("explicit bytes changed")
	}
}

func TestDocumentedFixtureCommand(t *testing.T) {
	args := []string{"-base", "testdata/base.js", "-candidate", "testdata/renamed.js", "-base-constructor", "createRegistry", "-candidate-constructor", "f", "-base-reader", "readRegistry", "-candidate-reader", "r", "-base-update", "updateRegistry", "-candidate-update", "u"}
	var out bytes.Buffer
	if exit := run(args, &out); exit != 0 {
		t.Fatalf("documented comparison exit=%d: %s", exit, out.String())
	}
	var r report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	requireControls(t, r)
}

func TestAdditionalMalformedGrammar(t *testing.T) {
	for _, source := range []string{
		"function f(){for(const x;;){}}", "function f(){for(let x=1 of data){}}", "function f(){for(let x,y of data){}}", "function f(){for(x+y of data){}}", "function f(){let {x};}", "function f(){let {...{x}}=data;}",
		"class C{constructor(){}constructor(){}}", "class C{constructor=1;}", "class C{static prototype(){}}", "class C{static \"prototype\"=1;}", "class C{async \"constructor\"(){}}",
		"function f(){return {__proto__:1,'__proto__':2}}", "function f(){return {__proto__:1,'\\x5f_proto__':2}}", "function f(){switch(x){x=2}}", "function f(){if(x)let y=1;}", "function f(){while(x)class C{}}", "function f(a=1){'use strict';return a}", "function f(){ {let x;var x;} }",
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := parse(context.Background(), []byte(source)); err == nil {
				t.Fatal("malformed grammar accepted")
			}
		})
	}
}

func TestPropertySpellingsAndClassMethodLookup(t *testing.T) {
	base := strings.Replace(synthetic, "static from(input)", "static 'fr\\x6fm'(input)", 1)
	base = strings.Replace(base, "return helper(input)}function patchRecord", "return Record.from(input)}function patchRecord", 1)
	r := evaluate(context.Background(), []byte(base), []byte(base), baseAnchors, baseAnchors, 60)
	if r.Status != equal {
		t.Fatalf("escaped method lookup: %+v", r)
	}
	other := strings.Replace(base, "'fr\\x6fm'", "'from'", 1)
	got, status := comparePrograms(parsed(t, base), parsed(t, other), baseAnchors, baseAnchors)
	if status != different || got[1].Callees[0].Status != different {
		t.Fatalf("property spelling was not preserved: %+v", got)
	}
}

func TestNamedMetadataContexts(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"function create(input){", "function create(input){const VERSION=\"one\";input.BUILD_TIME=\"day\";"},
		{"constructor(input){this.value=input}", "constructor(input){this.value=input;this.GIT_SHA=\"one\";}"},
		{"constructor(input){this.value=input}", "VERSION=\"one\";constructor(input){this.value=input}"},
	} {
		base := strings.Replace(synthetic, tc.old, tc.new, 1)
		other := strings.NewReplacer("=\"one\"", "=\"two\"", "=\"day\"", "=\"night\"").Replace(base)
		_, status := comparePrograms(parsed(t, base), parsed(t, other), baseAnchors, baseAnchors)
		if status != equal {
			t.Fatalf("named primitive value not masked in supported context: %s", tc.new)
		}
	}
}

func TestDeclaredCalleeDepthBoundary(t *testing.T) {
	base := strings.Replace(synthetic, "return{payload:input,tag:\"record\"}", "return alternate(input)", 1)
	other := strings.Replace(base, "tag:\"different\"", "tag:\"outside-first-level\"", 1)
	got, status := comparePrograms(parsed(t, base), parsed(t, other), baseAnchors, baseAnchors)
	if status != equal || got[0].Callees[0].BaseCallsBeyondLevel != 1 || got[0].Callees[0].CandidateCallsBeyondLevel != 1 {
		t.Fatalf("declared depth changed or lost its count: %+v", got)
	}
}

func TestControlsUseValuesNotQuotedKeys(t *testing.T) {
	const helper = "function h(x){return {outside:\"callee-value\",data:x}}"
	const tail = "function r(x){return h(x)}function u(x){return x}"
	for _, body := range []string{
		"return {'pid':x,'data':h(x),'text':`only ${x}`}",
		"return {'pid':x,'data':h(x),'flag':true}",
		"return {x,data:h(x),flag:null}",
	} {
		base := helper + "function f(x){" + body + "}" + tail
		r := evaluate(context.Background(), []byte(base), []byte(base), anchors{"f", "r", "u"}, anchors{"f", "r", "u"}, 60)
		if r.Status != equal {
			t.Fatalf("available quoted/shorthand/template/boolean controls: %+v", r)
		}
		requireControls(t, r)
	}
	base := helper + "function f(x){return {'pid':x,'data':h(x)}}" + tail
	r := evaluate(context.Background(), []byte(base), []byte(base), anchors{"f", "r", "u"}, anchors{"f", "r", "u"}, 60)
	if r.Status != "CONTROL_FAILED" || r.Controls[1].Reason != "no_eligible_plant_site" {
		t.Fatalf("property keys must not masquerade as value control sites: %+v", r)
	}
}

func TestStaticClassSwapControl(t *testing.T) {
	base := "class C{static read(x){return {tag:\"class\",data:x}}}function f(x){return {'pid':x,'item':C.read(x),'flag':true}}function r(x){return C.read(x)}function u(x){return x}"
	r := evaluate(context.Background(), []byte(base), []byte(base), anchors{"f", "r", "u"}, anchors{"f", "r", "u"}, 60)
	if r.Status != equal {
		t.Fatalf("static class swap control unavailable: %+v", r)
	}
	requireControls(t, r)
}
func FuzzParserBounded(f *testing.F) {
	for _, s := range []string{synthetic, "function f(){return `x${/}/}`}", "function f(){return 1/2}", "function f(){/*"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			t.Skip("bounded fuzz input")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p, err := parse(ctx, data)
		if err == nil && p == nil {
			t.Fatal("nil successful parse")
		}
	})
}
