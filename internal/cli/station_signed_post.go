package cli

import (
	"encoding/base64"
	"io"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
)

// readPostAttestation uses the existing streamed input mechanism on stations.
// It never opens a station-supplied pathname in the daemon's account.
func (a *App) readPostAttestation(operand string) (string, error) {
	f, err := a.openInputFile(operand[1:])
	if err != nil {
		return "", signedpost.InvalidAttestation
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, signedpost.MaxAttestationBytes+1))
	if err != nil || len(raw) == 0 {
		return "", signedpost.InvalidAttestation
	}
	if len(raw) > signedpost.MaxAttestationBytes {
		return "", signedpost.LargeAttestation
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// verifyStationPost runs before opening a session, network discovery, admission
// state or durable-post reservation. Exact room/topic IDs (or an explicit
// main-timeline reply) and full recipient IDs make the complete binding local.
// Ordinary, unmapped stations keep their existing alias and shorthand behavior.
func (a *App) verifyStationPost(room, topic, re string, main bool, kind, to, text, envelope, attestation string) error {
	if !matrix.ValidVerificationID(room, '!') || topic != "" && !matrix.ValidVerificationID(topic, '$') || topic == "" && (!main || re == "") {
		return signedpost.BindingUnavailable
	}
	var recipients []string
	for _, id := range splitList(to) {
		if id == "all" {
			continue
		}
		if !strings.HasPrefix(id, "@") || !matrix.ValidVerificationID(id, '@') {
			return signedpost.BindingUnavailable
		}
		recipients = append(recipients, id)
	}
	raw, err := matrix.DecodeEnvelope(envelope)
	if err != nil {
		return signedpost.InvalidEnvelope
	}
	return signedpost.Verify(raw, attestation, a.stationAuthorKey, signedpost.Draft{
		Room: room, Recipients: recipients, Kind: kind, Topic: topic, Re: re, Text: []byte(text),
	}, a.Now())
}
