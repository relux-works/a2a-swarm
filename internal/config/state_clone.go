package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// cloneState returns a State that shares no mutable memory with src. It is an
// explicit typed copy of every field, never a JSON round trip, so it neither
// repeats the decode cost nor alters opaque numbers or unknown members. Nil and
// non-nil empty containers keep their distinction; unexported writer
// diagnostics (StateWriter.ambiguous), invalid/forgiving markers, opaque
// members and the exact rawState input are retained. Strings and time.Time
// values are immutable and are copied by value.
//
// ok is false when a dynamic JSON container holds a value whose type the clone
// does not know; the caller then uses the ordinary decoder. A partial clone is
// never returned.
func cloneState(src *State) (State, bool) {
	if !stateCloneSchemaOK {
		return State{}, false
	}
	s := *src
	s.opaque = cloneRawMap(src.opaque)
	s.rawState = cloneBytes(src.rawState)
	s.Migration = copyValuePtr(src.Migration)
	s.Writer = copyValuePtr(src.Writer)
	if src.Monitor != nil {
		m := *src.Monitor
		m.Rung = cloneStrings(src.Monitor.Rung)
		s.Monitor = &m
	}
	s.MonitorOwner = copyValuePtr(src.MonitorOwner)
	s.MonitorConfig = copyValuePtr(src.MonitorConfig)
	s.MailAlarmConfig = copyValuePtr(src.MailAlarmConfig)
	s.MailAlarm.Jobs = cloneValues(src.MailAlarm.Jobs)
	if src.HostEnrollment != nil {
		e := *src.HostEnrollment
		if src.HostEnrollment.Attempts != nil {
			a := *src.HostEnrollment.Attempts
			a.Entries = cloneValues(src.HostEnrollment.Attempts.Entries)
			e.Attempts = &a
		}
		s.HostEnrollment = &e
	}
	s.Notification.Reservation = copyValuePtr(src.Notification.Reservation)
	s.AgentDMContexts = cloneRaw(src.AgentDMContexts)
	s.LegacyPending = cloneRaw(src.LegacyPending)
	s.LegacyAcked = cloneStrings(src.LegacyAcked)
	s.Threads = cloneStrings(src.Threads)
	s.ThreadEvents = cloneValues(src.ThreadEvents)
	s.Muted = cloneStrings(src.Muted)
	if src.LegacySubscriptions != nil {
		m := make(map[string][]string, len(src.LegacySubscriptions))
		for k, v := range src.LegacySubscriptions {
			m[k] = cloneStrings(v)
		}
		s.LegacySubscriptions = m
	}
	if src.HookSessions != nil {
		h := make([]HookSession, len(src.HookSessions))
		for i, x := range src.HookSessions {
			x.Rung = cloneStrings(x.Rung)
			h[i] = x
		}
		s.HookSessions = h
	}
	s.Lease = copyValuePtr(src.Lease)
	s.Runtime = copyValuePtr(src.Runtime)
	if src.Terminal != nil {
		t, ok := cloneTerminal(src.Terminal)
		if !ok {
			return State{}, false
		}
		s.Terminal = t
	}
	s.Wake = src.Wake.Clone()
	s.Liveness = cloneAgentStatusPtr(src.Liveness)
	if src.RuntimeProfilePending != nil {
		r := *src.RuntimeProfilePending
		r.Profile = cloneAgentStatus(src.RuntimeProfilePending.Profile)
		s.RuntimeProfilePending = &r
	}
	s.NoticeStatus = cloneAgentStatusPtr(src.NoticeStatus)
	s.NoticeRetry = copyValuePtr(src.NoticeRetry)
	s.OwnStatusWrite = copyValuePtr(src.OwnStatusWrite)
	if src.LivenessAlerts != nil {
		m := make(map[string]LivenessAlert, len(src.LivenessAlerts))
		for k, v := range src.LivenessAlerts {
			if v.Pending != nil {
				p, ok := cloneAlertMessage(*v.Pending)
				if !ok {
					return State{}, false
				}
				v.Pending = &p
			}
			m[k] = v
		}
		s.LivenessAlerts = m
	}
	return s, true
}

func cloneTerminal(src *TerminalBinding) (*TerminalBinding, bool) {
	t := *src
	t.OwnNotice = cloneOwnNotice(src.OwnNotice)
	if src.NoticeReport != nil {
		r, ok := cloneNoticeReport(*src.NoticeReport)
		if !ok {
			return nil, false
		}
		t.NoticeReport = &r
	}
	if src.ConsumerReport != nil {
		r, ok := cloneNoticeReport(*src.ConsumerReport)
		if !ok {
			return nil, false
		}
		t.ConsumerReport = &r
	}
	t.NoticeOrigin = copyValuePtr(src.NoticeOrigin)
	t.NoticePosition = copyValuePtr(src.NoticePosition)
	t.Recovery = copyValuePtr(src.Recovery)
	t.SendRefusal = copyValuePtr(src.SendRefusal)
	t.Readiness = copyValuePtr(src.Readiness)
	t.AttemptIDs = cloneStrings(src.AttemptIDs)
	t.Rung = cloneStrings(src.Rung)
	t.Reservations = cloneValues(src.Reservations)
	t.Idle = copyValuePtr(src.Idle)
	return &t, true
}

func cloneNoticeReport(r NoticeReport) (NoticeReport, bool) {
	m, ok := cloneAlertMessage(r.Message)
	r.Message = m
	return r, ok
}

func cloneAlertMessage(m LivenessAlertMessage) (LivenessAlertMessage, bool) {
	if m.Content == nil {
		return m, true
	}
	c, ok := cloneJSONObject(m.Content)
	m.Content = c
	return m, ok
}

func cloneOwnNotice(o OwnNoticeObservation) OwnNoticeObservation {
	o.PossiblyOrphaned = copyValuePtr(o.PossiblyOrphaned)
	return o
}

func cloneAgentStatus(a AgentStatus) AgentStatus {
	a.OwnNotice = cloneOwnNotice(a.OwnNotice)
	return a
}

func cloneAgentStatusPtr(a *AgentStatus) *AgentStatus {
	if a == nil {
		return nil
	}
	c := cloneAgentStatus(*a)
	return &c
}

// cloneJSONObject copies a map produced by encoding/json into map[string]any.
func cloneJSONObject(m map[string]any) (map[string]any, bool) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		c, ok := cloneJSONValue(v)
		if !ok {
			return nil, false
		}
		out[k] = c
	}
	return out, true
}

func cloneJSONValue(v any) (any, bool) {
	switch x := v.(type) {
	case nil, bool, float64, string, json.Number:
		return x, true
	case map[string]any:
		if x == nil {
			return x, true
		}
		return cloneJSONObject(x)
	case []any:
		if x == nil {
			return x, true
		}
		out := make([]any, len(x))
		for i, e := range x {
			c, ok := cloneJSONValue(e)
			if !ok {
				return nil, false
			}
			out[i] = c
		}
		return out, true
	}
	return nil, false
}

// copyValuePtr copies a pointed record whose fields are all values (scalars,
// strings, time values or value structs of those). The schema pin and the
// clone coverage tests reject a new mutable field in any such record.
func copyValuePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneValues copies a slice whose elements are value-only records.
func cloneValues[T any](v []T) []T {
	if v == nil {
		return nil
	}
	return append(make([]T, 0, len(v)), v...)
}

func cloneStrings(v []string) []string { return cloneValues(v) }

func cloneBytes(b []byte) []byte { return cloneValues(b) }

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return json.RawMessage(cloneBytes(b))
}

func cloneRawMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = cloneRaw(v)
	}
	return out
}

// stateCloneSchemaPin is the reviewed State type graph that cloneState covers.
// A new or changed field anywhere in that graph changes the signature: the
// scoped decode memo and the writer working-copy clone are then disabled (the
// ordinary decoder runs) until the clone is reviewed and this pin updated.
const stateCloneSchemaPin = "c7fab2308554db5d830ab890a0f85202e6ef025d3175d2ce92ef5d612d04bfc5"

var stateCloneSchemaOK = stateSchemaReviewed(reflect.TypeFor[State](), stateCloneSchemaPin)

// stateSchemaReviewed reports whether the reflected type graph of root is the
// one recorded by pin. Only a match enables reuse.
func stateSchemaReviewed(root reflect.Type, pin string) bool {
	return stateSchemaSignatureOf(root) == pin
}

// stateSchemaSignature hashes the type graph reachable from State. It reads
// types only, never values.
func stateSchemaSignature() string { return stateSchemaSignatureOf(reflect.TypeFor[State]()) }

// stateSchemaSignatureOf hashes one canonical, recursive encoding of the type
// graph reachable from root (schemaEncoder). It reads types only, never values.
func stateSchemaSignatureOf(root reflect.Type) string {
	e := schemaEncoder{ids: map[reflect.Type]int{}}
	e.encode(root)
	h := sha256.Sum256([]byte(e.b.String()))
	return hex.EncodeToString(h[:])
}

// schemaEncoder writes the canonical encoding of a type graph. One recursive
// rule covers every reflect.Kind, so a component type is always spelled by its
// own identity and structure, never by reflect.Type.String:
//
//   - a named type (including predeclared ones such as int and error) is
//     written as named#<id>(<package path> <name>)=<its structure> at its first
//     occurrence and as @<id> afterwards; ids count named types in visit
//     order, so recursion terminates and two distinct types that share a
//     package path and name stay distinguishable;
//   - pointer and slice: element; array: length and element; map: key and
//     element; chan: direction and element;
//   - struct: every field in declaration order with its index, name, package
//     path (exported or not), embedding flag, tag and type;
//   - interface: its full method set (embedded interfaces contribute through
//     it) with each method's name, package path and function type;
//   - func: variadic flag, then every input and every output type in order;
//   - basic kinds: the kind (the named prefix carries the identity).
//
// time.Time is an immutable leaf; its private layout is not part of the State
// schema. Method sets of non-interface named types are not encoded: they do not
// change the data layout that cloneState copies.
type schemaEncoder struct {
	b   strings.Builder
	ids map[reflect.Type]int
}

func (e *schemaEncoder) encode(t reflect.Type) {
	if t.Name() != "" {
		if id, ok := e.ids[t]; ok {
			fmt.Fprintf(&e.b, "@%d", id)
			return
		}
		id := len(e.ids)
		e.ids[t] = id
		fmt.Fprintf(&e.b, "named#%d(%q %q)=", id, t.PkgPath(), t.Name())
		if t == timeType {
			e.b.WriteString("immutable-leaf")
			return
		}
	}
	k := t.Kind()
	e.b.WriteString(k.String())
	switch k {
	case reflect.Pointer, reflect.Slice:
		e.wrap("(", t.Elem(), ")")
	case reflect.Array:
		fmt.Fprintf(&e.b, "[len=%d]", t.Len())
		e.wrap("(", t.Elem(), ")")
	case reflect.Chan:
		fmt.Fprintf(&e.b, "[dir=%d]", int(t.ChanDir()))
		e.wrap("(", t.Elem(), ")")
	case reflect.Map:
		e.wrap("(key=", t.Key(), "")
		e.wrap(",elem=", t.Elem(), ")")
	case reflect.Struct:
		fmt.Fprintf(&e.b, "{fields=%d", t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fmt.Fprintf(&e.b, ";field %d %q pkg=%q embedded=%t tag=%q type=", i, f.Name, f.PkgPath, f.Anonymous, string(f.Tag))
			e.encode(f.Type)
		}
		e.b.WriteString("}")
	case reflect.Interface:
		fmt.Fprintf(&e.b, "{methods=%d", t.NumMethod())
		for i := 0; i < t.NumMethod(); i++ {
			m := t.Method(i)
			fmt.Fprintf(&e.b, ";method %q pkg=%q type=", m.Name, m.PkgPath)
			e.encode(m.Type)
		}
		e.b.WriteString("}")
	case reflect.Func:
		fmt.Fprintf(&e.b, "(variadic=%t in=%d", t.IsVariadic(), t.NumIn())
		for i := 0; i < t.NumIn(); i++ {
			e.wrap(";", t.In(i), "")
		}
		fmt.Fprintf(&e.b, " out=%d", t.NumOut())
		for i := 0; i < t.NumOut(); i++ {
			e.wrap(";", t.Out(i), "")
		}
		e.b.WriteString(")")
	}
}

func (e *schemaEncoder) wrap(open string, t reflect.Type, close string) {
	e.b.WriteString(open)
	e.encode(t)
	e.b.WriteString(close)
}

// stateCharge is a conservative retained-data admission metric for one parsed
// State graph: fixed record sizes, string and buffer bytes, slice backing
// capacity, map entries and nested interface graphs, each with bookkeeping
// overhead. It is not a claim about Go heap/RSS or allocator overhead. Shared
// immutable runtime metadata (time.Location, type descriptors) is excluded.
type stateCharge struct {
	bytes, nodes int64
	unsupported  bool
}

const (
	chargeObjectOverhead = 16
	chargeSliceHeader    = 24
	chargeMapBase        = 64
	chargeMapEntry       = 32
)

var timeType = reflect.TypeFor[time.Time]()

// parsedStateCharge measures the parsed graph of s, excluding the rawState
// input, which the memo charges against its raw-byte bound. It only reads.
func parsedStateCharge(s *State) stateCharge {
	var c stateCharge
	v := reflect.ValueOf(s).Elem()
	c.bytes += int64(v.Type().Size()) + chargeObjectOverhead
	c.nodes++
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Name == "rawState" {
			continue
		}
		c.walk(v.Field(i))
	}
	return c
}

func (c *stateCharge) walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		// Counted in the containing record's fixed size.
	case reflect.String:
		if n := v.Len(); n > 0 {
			c.bytes += int64(n) + chargeObjectOverhead
			c.nodes++
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		c.bytes += int64(v.Type().Elem().Size()) + chargeObjectOverhead
		c.nodes++
		c.walk(v.Elem())
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			c.walk(v.Field(i))
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			c.walk(v.Index(i))
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		c.bytes += int64(v.Cap())*int64(v.Type().Elem().Size()) + chargeSliceHeader
		c.nodes++
		for i := 0; i < v.Len(); i++ {
			c.walk(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		c.bytes += chargeMapBase
		c.nodes++
		per := int64(v.Type().Key().Size()+v.Type().Elem().Size()) + chargeMapEntry
		it := v.MapRange()
		for it.Next() {
			c.bytes += per
			c.nodes++
			c.walk(it.Key())
			c.walk(it.Value())
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		e := v.Elem()
		c.bytes += int64(e.Type().Size()) + chargeObjectOverhead
		c.nodes++
		c.walk(e)
	default:
		// Channels, functions and unsafe pointers are not State data.
		c.unsupported = true
	}
}
