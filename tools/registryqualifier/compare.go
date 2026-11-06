package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

const equal = "EQUAL"
const different = "DIFFERENT"
const notCompared = "NOT_COMPARED"

type anchors [3]string

var roles = [3]string{"constructor", "reader", "partial_update"}

type span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type comparison struct {
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Base          *span  `json:"base_span,omitempty"`
	Candidate     *span  `json:"candidate_span,omitempty"`
	BaseHash      string `json:"base_normalized_sha256,omitempty"`
	CandidateHash string `json:"candidate_normalized_sha256,omitempty"`
}
type calleeComparison struct {
	Index int `json:"index"`
	comparison
	BaseCallsBeyondLevel      int `json:"base_calls_beyond_level"`
	CandidateCallsBeyondLevel int `json:"candidate_calls_beyond_level"`
}
type functionComparison struct {
	Role string `json:"role"`
	comparison
	Body    string             `json:"body_status,omitempty"`
	Callees []calleeComparison `json:"callees"`
}
type controlResult struct {
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Planted      bool   `json:"planted"`
	Detected     bool   `json:"detected"`
	MutantSHA256 string `json:"mutant_sha256,omitempty"`
	FunctionRole string `json:"function_role,omitempty"`
	Observation  string `json:"observation,omitempty"`
}
type report struct {
	Format          string               `json:"format"`
	Status          string               `json:"status"`
	Reason          string               `json:"reason,omitempty"`
	BaseSHA256      string               `json:"base_sha256,omitempty"`
	CandidateSHA256 string               `json:"candidate_sha256,omitempty"`
	Functions       []functionComparison `json:"functions"`
	Controls        []controlResult      `json:"controls"`
	Limits          limits               `json:"limits"`
	Limitations     []string             `json:"limitations"`
}
type limits struct {
	FileBytes        int `json:"file_bytes"`
	Tokens           int `json:"tokens_per_bundle"`
	Depth            int `json:"parse_depth"`
	Definitions      int `json:"definitions_per_bundle"`
	CallReferences   int `json:"call_references_per_bundle"`
	NormalizedTokens int `json:"normalized_tokens_per_bundle"`
	Seconds          int `json:"runtime_seconds"`
}

func newReport(seconds int) report {
	return report{Format: "registryqualifier/1", Status: notCompared, Functions: []functionComparison{}, Controls: []controlResult{},
		Limits: limits{maxBytes, maxTokens, maxDepth, maxDefinitions, 100_000, 8_000_000, seconds},
		Limitations: []string{
			"Static structural comparison of explicitly selected functions; no JavaScript execution and no formal semantic equivalence claim.",
			"Caller must independently identify all three complete registry functions in each bundle and establish the base's qualification and provenance.",
			"Only immediate uniquely resolved function/class callees are compared; resolved calls beyond that level are counted, not expanded.",
			"Unsupported syntax, dynamic/member calls without a unique local class target, mutable targets and unresolved calls refuse comparison.",
			"Free non-call data references are consistently alpha-renamed; their external values, other bundle code, effects and live registry behavior are outside this comparison.",
			"Property and literal spellings remain byte-exact; only immediate primitive VERSION, BUILD_TIME and GIT_SHA values are masked.",
			"No release qualification, allowlist update, live-record access or provider behavior is established by this report.",
		}}
}
func checksum(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func (p *program) extent(d *definition) *span {
	return &span{p.tokens[d.start].lo, p.tokens[d.end-1].hi}
}

func compareDefinition(a *program, ad *definition, b *program, bd *definition) comparison {
	ax, bx := a.normalized(ad), b.normalized(bd)
	s := equal
	if ax != bx || ad.kind != bd.kind {
		s = different
	}
	return comparison{Status: s, Base: a.extent(ad), Candidate: b.extent(bd), BaseHash: ax, CandidateHash: bx}
}
func severity(a, b string) string {
	if a == notCompared || b == notCompared {
		return notCompared
	}
	if a == different || b == different {
		return different
	}
	return equal
}
func checkCalls(p *program, d *definition) string {
	for _, c := range d.calls {
		if _, reason := p.target(c); reason != "" {
			return reason
		}
	}
	return ""
}
func comparePrograms(a, b *program, aa, ba anchors) ([]functionComparison, string) {
	results := make([]functionComparison, 0, 3)
	status := equal
	seenA, seenB := map[*definition]bool{}, map[*definition]bool{}
	for i, role := range roles {
		r := functionComparison{Role: role, comparison: comparison{Status: notCompared}, Callees: []calleeComparison{}}
		ad, ar := a.find(aa[i])
		bd, br := b.find(ba[i])
		if ar != "" || br != "" {
			r.Reason = "base_" + ar
			if ar == "" {
				r.Reason = "candidate_" + br
			}
		} else if seenA[ad] || seenB[bd] {
			r.Reason = "aliased_role_anchor"
		} else if ad.kind == "class" || bd.kind == "class" {
			r.Reason = "anchor_is_class_not_function"
		} else {
			seenA[ad] = true
			seenB[bd] = true
			r.comparison = compareDefinition(a, ad, b, bd)
			r.Body = r.Status
			count := len(ad.calls)
			if len(bd.calls) > count {
				count = len(bd.calls)
			}
			if count > 4096 {
				refusal("comparison_call_limit")
			}
			for n := 0; n < count; n++ {
				c := calleeComparison{Index: n, comparison: comparison{Status: notCompared}}
				if n >= len(ad.calls) || n >= len(bd.calls) {
					c.Reason = "missing_corresponding_callee"
				} else {
					at, ar := a.target(ad.calls[n])
					bt, br := b.target(bd.calls[n])
					if ar != "" || br != "" {
						c.Reason = "base_" + ar
						if ar == "" {
							c.Reason = "candidate_" + br
						}
					} else {
						c.comparison = compareDefinition(a, at, b, bt)
						c.BaseCallsBeyondLevel = len(at.calls)
						c.CandidateCallsBeyondLevel = len(bt.calls)
						if ar = checkCalls(a, at); ar != "" {
							c.Status = notCompared
							c.Reason = "base_" + ar
						} else if br = checkCalls(b, bt); br != "" {
							c.Status = notCompared
							c.Reason = "candidate_" + br
						}
					}
				}
				r.Callees = append(r.Callees, c)
				r.Status = severity(r.Status, c.Status)
			}
		}
		status = severity(status, r.Status)
		results = append(results, r)
	}
	return results, status
}

// evaluate always runs actual mutations of the supplied base. A parse failure,
// missing anchor, or unavailable control cannot produce an equal report.
func evaluate(ctx context.Context, base, candidate []byte, aa, ba anchors, seconds int) (out report) {
	out = newReport(seconds)
	out.BaseSHA256 = checksum(base)
	out.CandidateSHA256 = checksum(candidate)
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(parseError); ok {
				out.Status = notCompared
				out.Reason = string(e)
				fillFailedControls(&out, "comparison_refused")
			} else {
				panic(r)
			}
		}
	}()
	a, err := parse(ctx, base)
	if err != nil {
		out.Reason = "base_" + err.Error()
		fillFailedControls(&out, "base_parse_refused")
		return
	}
	b, err := parse(ctx, candidate)
	if err != nil {
		out.Reason = "candidate_" + err.Error()
		out.Controls = runControls(ctx, a, aa)
		return
	}
	out.Functions, out.Status = comparePrograms(a, b, aa, ba)
	out.Controls = runControls(ctx, a, aa)
	for _, c := range out.Controls {
		if c.Status != "PASS" {
			out.Status = "CONTROL_FAILED"
			out.Reason = "required_control_failed"
			break
		}
	}
	if ctx.Err() != nil {
		out.Status = notCompared
		out.Reason = "deadline"
	}
	return
}
func fillFailedControls(r *report, reason string) {
	for _, kind := range []string{"property_rename", "literal_change", "callee_swap"} {
		found := false
		for _, c := range r.Controls {
			found = found || c.Kind == kind
		}
		if !found {
			r.Controls = append(r.Controls, controlResult{Kind: kind, Status: "FAIL", Reason: reason})
		}
	}
}
func splice(data []byte, lo, hi int, replacement string) []byte {
	if len(data)+len(replacement)-(hi-lo) > maxBytes {
		return nil
	}
	b := make([]byte, 0, len(data)+len(replacement)-(hi-lo))
	b = append(b, data[:lo]...)
	b = append(b, replacement...)
	b = append(b, data[hi:]...)
	return b
}

func runControls(ctx context.Context, a *program, aa anchors) []controlResult {
	_, status := comparePrograms(a, a, aa, aa)
	controls := make([]controlResult, 0, 3)
	for _, kind := range []string{"property_rename", "literal_change", "callee_swap"} {
		c := controlResult{Kind: kind, Status: "FAIL"}
		if status != equal {
			c.Reason = "base_not_comparable"
			controls = append(controls, c)
			continue
		}
		var mutated []byte
		role := -1
		edge := -1
		for i := range aa {
			d, _ := a.find(aa[i])
			if kind == "callee_swap" {
				for n, call := range d.calls {
					if call.ref < 0 {
						continue
					}
					target, reason := a.target(call)
					if reason != "" {
						continue
					}
					name := "__registryQualifierPlantedCallee"
					for strings.Contains(string(a.data), name) {
						name += "X"
						if len(name) > 128 {
							break
						}
					}
					if len(name) > 128 {
						continue
					}
					if target.kind == "class" && !call.construct && call.member == "" {
						continue
					}
					t := a.tokens[call.ref]
					mutated = splice(a.data, t.lo, t.hi, name)
					plant := ";function " + name + "(){return {__registryQualifierPlantedProperty:\"control\"};}"
					if target.kind == "class" {
						plant = ";class " + name + "{constructor(){this.__registryQualifierPlantedProperty=\"control\";}}"
						if call.member != "" {
							plant = ";class " + name + "{static " + call.member + "(){return {__registryQualifierPlantedProperty:\"control\"};}}"
						}
					}
					if len(mutated)+len(plant) > maxBytes {
						mutated = nil
						continue
					}
					mutated = append(mutated, plant...)
					role = i
					edge = n
					break
				}
			} else {
				for _, t := range a.tokens[d.start:d.end] {
					if t.mask {
						continue
					}
					if kind == "property_rename" && (t.role == "property" || t.role == "shorthand") && !metadata(t.raw) {
						replacement := t.raw + "__registryQualifierControl"
						if t.kind == "string" || t.kind == "number" {
							replacement = strconv.Quote(propertyKey(t.raw) + "__registryQualifierControl")
						}
						if t.role == "shorthand" {
							replacement = replacement + ":" + t.raw
						}
						mutated = splice(a.data, t.lo, t.hi, replacement)
						role = i
						break
					}
					if kind == "literal_change" && t.role != "property" && t.role != "shorthand" {
						replacement := ""
						switch t.kind {
						case "string":
							replacement = "\"__registryQualifierLiteralControl\""
						case "number":
							replacement = "987654321"
						case "regexp":
							replacement = "/__registryQualifierRegexControl/"
						case "template_part", "template_end":
							if strings.HasPrefix(t.raw, "`") {
								replacement = "`__registryQualifierTemplateControl" + t.raw[1:]
							} else {
								replacement = "__registryQualifierTemplateControl" + t.raw
							}
						case "keyword":
							if t.raw == "true" {
								replacement = "false"
							} else if t.raw == "false" {
								replacement = "true"
							} else if t.raw == "null" {
								replacement = "true"
							}
						}
						if replacement != "" && replacement != t.raw {
							mutated = splice(a.data, t.lo, t.hi, replacement)
							role = i
							break
						}
					}
				}
			}
			if mutated != nil {
				break
			}
		}
		if mutated == nil {
			c.Reason = "no_eligible_plant_site"
			controls = append(controls, c)
			continue
		}
		c.Planted = true
		c.MutantSHA256 = checksum(mutated)
		c.FunctionRole = roles[role]
		mutant, err := parse(ctx, mutated)
		if err != nil {
			c.Reason = "mutant_parse_refused"
			controls = append(controls, c)
			continue
		}
		got, all := comparePrograms(a, mutant, aa, aa)
		c.Observation = all
		detected := got[role].Body == different
		if kind == "callee_swap" {
			detected = edge < len(got[role].Callees) && got[role].Callees[edge].Status == different
		}
		// A mutation may also change another selected function's callee. It
		// must change the intended body/edge, rather than merely refuse parsing.
		if all == different && detected && !bytes.Equal(a.data, mutated) {
			c.Status = "PASS"
			c.Detected = true
		} else {
			c.Reason = "intended_difference_not_detected"
		}
		controls = append(controls, c)
	}
	return controls
}
