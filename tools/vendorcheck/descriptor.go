package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// This gate supports exactly the a2a command closure, not arbitrary drivers or
// extra targets. Read the selected commit's blob, never the worktree descriptor.
func checkDescriptor(ctx context.Context, repo string, selected map[string]entry) error {
	item, ok := selected["skill-build.json"]
	if !ok {
		return errors.New("build_descriptor_missing")
	}
	data, err := git(ctx, repo, nil, "cat-file", "blob", item.oid)
	if err != nil {
		return errors.New("build_descriptor_unavailable: skill-build.json")
	}
	want := map[string]any{
		"schema_version": json.Number("1"),
		"targets": map[string]any{"a2a": map[string]any{
			"driver": "go-repository-v1", "build_root": ".", "source_dir": "cmd/a2a",
		}},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !descriptorValue(decoder, want) {
		return errors.New("build_descriptor_invalid: skill-build.json")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("build_descriptor_invalid: skill-build.json")
	}
	return nil
}

// Match objects independent of key order, rejecting duplicate and unknown keys.
func descriptorValue(decoder *json.Decoder, want any) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	fields, object := want.(map[string]any)
	if !object {
		return token == want
	}
	if token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, stringKey := token.(string)
		value, exists := fields[key]
		if err != nil || !stringKey || !exists || seen[key] || !descriptorValue(decoder, value) {
			return false
		}
		seen[key] = true
	}
	end, err := decoder.Token()
	return err == nil && end == json.Delim('}') && len(seen) == len(fields)
}
