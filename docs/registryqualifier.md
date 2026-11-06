# Static registry bundle comparison

`tools/registryqualifier` is a standalone Go repository tool. It reads an
independently qualified base bundle and a candidate bundle as inert bytes. It
never executes JavaScript, loads a module, links a bundle, starts a provider,
searches installed files, or opens a session registry. It does not change the
production CLI or its qualification rules. Linux and macOS are supported.

Identify the complete record constructor, reader and partial-update function in
each file by static inspection first. Supply their exact lexical binding names;
minified names may differ between files. These six explicit selectors prevent a
missing or changed anchor from silently matching a nearby fragment. Each must
identify exactly one function in the entire parsed bundle, and the three roles
must be distinct. Function declarations, and simple variable bindings initialized
with function expressions or arrows, are eligible. Methods alone are not anchors.
The tool does not infer that a selected function implements a registry role or
prove the supplied base's provenance or qualification.

From the repository root, this single reproducible command compares the supplied
synthetic examples and writes the JSON report:

```sh
go run ./tools/registryqualifier \
  -base ./tools/registryqualifier/testdata/base.js \
  -candidate ./tools/registryqualifier/testdata/renamed.js \
  -base-constructor createRegistry -candidate-constructor f \
  -base-reader readRegistry -candidate-reader r \
  -base-update updateRegistry -candidate-update u \
  -seconds 60 > registry-comparison.json
```

Replace both paths and all six names with the independently inspected files and
bindings for a release comparison. There are no default paths, environment
selectors, discovery, downloads or external runtimes. Only explicit regular files
are accepted; final symlinks are refused, including at open time. Inputs are never
rewritten. Reports contain fixed role/reason codes, byte ranges, counts and SHA-256
digests, without paths, source snippets, identifier names, property names or raw
literal values. Keep the report with the exact input hashes. A process deadline
also emits a closed failure report to stderr if normal completion is blocked.

## Comparison

The bounded recursive-descent parser reads the whole supported source grammar
and retains complete function intervals, including names, parameters, nested
definitions, strings, regexps and template substitutions. It does not search with
a function-shaped regular expression or cut a body at a brace inside a literal.
Slash is interpreted in expression context, so division and regex literals are
different tokens. Nested template substitutions return to the expression parser.

Within each complete definition, identifiers are consistently alpha-renamed by
lexical binding, with separate identities for shadowed bindings. Unbound data
identifiers retain their within-function identity pattern through alpha-renaming.
Keywords, operators, property keys, member names, numeric/string/regexp spellings,
template text and newline presence remain exact. Shorthand keys retain their
original property name as well as the renamed binding. Whitespace other than
newlines and comments are omitted.

Only immediate primitive values of exact `VERSION`, `BUILD_TIME` and `GIT_SHA`
names are masked, in declarations, object fields, class fields or direct member
assignments. The property name remains. Quoted object keys with those exact
spellings are included. Other names, lowercase `version`, computed names,
expressions and templates with substitutions are not masked. No broad timestamp,
version-like string, hash-like string or path masking is performed.

Each call in a selected function is paired in source order with the corresponding
call in the other file. Direct identifier calls and `new` targets must resolve to
one immutable function or class binding in lexical scope. A static class method
call can resolve to its complete class definition; the method name stays exact.
The complete target is normalized and compared for one level. Calls inside that
target must also resolve; their bodies are not expanded again and their counts
are reported as calls beyond the comparison level. Dynamic calls, parameter
callbacks, computed targets, unknown member receivers, reassigned bindings and
duplicate declarations cannot be assumed equal. Even familiar external calls
such as `JSON.parse` are unresolved unless represented by a uniquely resolvable
local definition. There is no intrinsic or provider allowlist.

Every valid comparison also compares the base with itself and plants three
separate mutations into the **actual supplied base bytes**:

1. Rename an actual property in a selected function, including quoted and
   shorthand keys.
2. Change an actual unmasked string, number, boolean/null, template text or regexp
   value. Quoted property keys cannot stand in for a value control.
3. Swap an actual direct callee or static class receiver to a fresh, differently
   implemented function or class inserted into the inert source.

Each mutant is parsed again and passed through the same comparator. The intended
body or callee edge must be `DIFFERENT`; parse refusal or a missing anchor is not
a detected control. A missing plant site, a non-comparable base, a refused mutant
or an undetected change fails the run. A comparator that reports every input
identical therefore cannot pass the controls. The controls are sensitivity checks,
not proof that all possible schema changes are detectable.

## Bounds and refusal

Hard bounds per input are 64 MiB, 1,000,000 tokens, 50,000 definitions, parse depth
256, and 100,000 accumulated call references. Comparison accepts at most 4,096
call edges per selected function and hashes at most 8,000,000 definition tokens
per parsed bundle, caching each definition's hash. Each regexp is at most 1 MiB.
The total runtime budget defaults to 60 seconds and accepts only 1–90 seconds.
These bounds cannot be increased by flags. Read size is checked before and after
opening; a changed identity or size refuses. Concurrent same-size writes are not
a snapshot guarantee: supply stable files and check their reported digests.

This is a rejecting JavaScript subset, not a general ECMAScript implementation.
It supports ASCII binding names; function declarations/expressions; simple arrow
parameters; ordinary and async methods; classes without inheritance; literal and
computed object properties; binding destructuring; common expression operators;
blocks, conditionals, loops, switch and try/catch. Unsupported syntax anywhere
in either bundle refuses the complete comparison. Examples include modules,
generators, optional chaining, inheritance, accessors, decorators, private names,
labels, `with`, `delete`, `in` and `for await`, complex/async-parenthesized arrow
parameters, Unicode/escaped identifiers and Unicode line separators. Regexps use
a validated common JavaScript/Go core: lookaround, backreferences, Unicode flags
and escapes outside that core refuse. No regex is executed. A refused bundle
needs a separately tested parser extension; do not preprocess or drop code to
make a comparison pass.

Top-level non-function data initializers, external values, dynamic effects,
resolved callees beyond the first level, the rest of the bundle's behavior, live
record contents and provider UI behavior are outside the comparison. Changed
minifier transformations can also cause conservative differences. `EQUAL` means
equal normalized structure within this declared scope, with all three planted
controls detected. It is **not formal JavaScript semantic equivalence**, release
approval or sufficient evidence to change a qualification rule.

Exit 0 requires all three functions and their first-level callees to compare
`EQUAL`, with all controls `PASS`. Exit 1 means a difference, `NOT_COMPARED`, parse
refusal or `CONTROL_FAILED`. Exit 2 means invalid arguments, unreadable or bounded
input refusal, unsupported platform, a process deadline or report write failure.
`go run` wraps a nonzero program exit; use a built binary if the precise code
matters. Any nonzero exit blocks a successful comparison. Invalid arguments and
input refusals include failed/unavailable control entries rather than an equality
claim. No live key/type capture or sanitized live-record fixture feature is
implemented.

## Tests

```sh
go test ./tools/registryqualifier
go test -race ./tools/registryqualifier
go vet ./tools/registryqualifier
```

All fixture bundles and identities are synthetic. Tests cover identifier
renaming, binding relationships and shadowing, literal/property drift throughout
complete bodies, metadata masking boundaries, function/class callee swaps,
ambiguous/missing anchors, unresolved and mutable calls, malformed/unsupported
source, resource bounds, planted controls, file refusals, and command exits with
source-free reports. Fuzz seeds exercise lexical boundaries without a JavaScript
runtime. The synthetic examples establish no provider release qualification.
