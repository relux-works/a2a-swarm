package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// AvailabilityOutcome describes registration availability at the instant of a
// GET observation. None of these outcomes is a general account-existence test.
type AvailabilityOutcome string

const (
	AvailabilityUnknown   AvailabilityOutcome = "unknown"
	AvailabilityAvailable AvailabilityOutcome = "available"
	AvailabilityTaken     AvailabilityOutcome = "taken"
	AvailabilityInvalid   AvailabilityOutcome = "invalid"
	AvailabilityReserved  AvailabilityOutcome = "application_service_reserved"
)

type AvailabilityResult struct {
	Outcome    AvailabilityOutcome `json:"outcome"`
	Reason     string              `json:"reason,omitempty"`
	HTTPStatus int                 `json:"http_status,omitempty"`
}

// Availability makes one anonymous GET to the original configured endpoint.
// It never invokes UserExists, registration, discovery, or a retry/fallback.
// Matrix v1.19: https://spec.matrix.org/v1.19/client-server-api/#get_matrixclientv3registeravailable
func (c *Client) Availability(ctx context.Context, username string) (AvailabilityResult, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	unknown := func(reason string, status int) (AvailabilityResult, error) {
		err := errors.New("matrix: availability unknown (" + reason + ")")
		if ctx.Err() != nil {
			err = fmt.Errorf("matrix: availability unknown (interrupted): %w", ctx.Err())
			reason = "interrupted"
		}
		return AvailabilityResult{Outcome: AvailabilityUnknown, Reason: reason, HTTPStatus: status}, err
	}
	if username == "" {
		return unknown("invalid_request", 0)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" || u.Scheme != "http" && u.Scheme != "https" {
		return unknown("invalid_endpoint", 0)
	}
	// Do not let an authenticated caller's token or cookie jar become authority
	// for this public query. Copies leave the caller's HTTP policy unchanged.
	anon := c.WithoutRedirects()
	anon.Token = ""
	hc := anon.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	copyHTTP := *hc
	copyHTTP.Jar = nil
	anon.HTTP = &copyHTTP
	resp, err := anon.send(ctx, http.MethodGet, v3+"/register/available", url.Values{"username": {username}}, nil, "", -1)
	if ctx.Err() != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return unknown("interrupted", 0)
	}
	if err != nil {
		var me *Error
		if !errors.As(err, &me) {
			return unknown("transport", 0)
		}
		if !me.OriginalRequest {
			return unknown("mismatched_response", me.Status)
		}
		if me.Status == http.StatusForbidden {
			return unknown("forbidden", me.Status)
		}
		if me.Status == http.StatusNotFound || me.Status == http.StatusMethodNotAllowed || me.Status == http.StatusNotImplemented {
			return unknown("unsupported", me.Status)
		}
		if me.Status == http.StatusTooManyRequests {
			return unknown("rate_limited", me.Status)
		}
		if me.Status >= 300 && me.Status < 400 {
			return unknown("redirect", me.Status)
		}
		if me.control == nil || !me.BodyComplete {
			return unknown("malformed_response", me.Status)
		}
		if _, contradictory := me.control.value["available"]; contradictory {
			return unknown("mismatched_response", me.Status)
		}
		if me.Status == http.StatusBadRequest {
			outcome := AvailabilityUnknown
			switch me.ErrCode {
			case "M_USER_IN_USE":
				outcome = AvailabilityTaken
			case "M_INVALID_USERNAME":
				outcome = AvailabilityInvalid
			case "M_EXCLUSIVE":
				outcome = AvailabilityReserved
			}
			if outcome != AvailabilityUnknown {
				return AvailabilityResult{Outcome: outcome, HTTPStatus: me.Status}, nil
			}
		}
		return unknown("http_error", me.Status)
	}
	v, err := readControl(ctx, resp, maxErrorBody)
	if ctx.Err() != nil {
		return unknown("interrupted", resp.StatusCode)
	}
	if err != nil {
		return unknown("malformed_response", resp.StatusCode)
	}
	available, ok := v.value["available"].(bool)
	_, hasErrcode := v.value["errcode"]
	_, hasError := v.value["error"]
	if resp.StatusCode != http.StatusOK || !ok || !available || hasErrcode || hasError {
		return unknown("mismatched_response", resp.StatusCode)
	}
	return AvailabilityResult{Outcome: AvailabilityAvailable, HTTPStatus: resp.StatusCode}, nil
}
