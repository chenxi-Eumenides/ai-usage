package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelErrorsDirect(t *testing.T) {
	if !IsAuthError(ErrAuth) {
		t.Error("IsAuthError(ErrAuth) = false")
	}
	if !IsRateLimited(ErrRateLimit) {
		t.Error("IsRateLimited(ErrRateLimit) = false")
	}
	if !IsParseError(ErrParse) {
		t.Error("IsParseError(ErrParse) = false")
	}
	if !IsNotSupported(ErrNotSupported) {
		t.Error("IsNotSupported(ErrNotSupported) = false")
	}
}

func TestSentinelErrorsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("%w: invalid key format", ErrAuth)
	if !IsAuthError(wrapped) {
		t.Error("IsAuthError(wrapped ErrAuth) = false")
	}
	if IsRateLimited(wrapped) {
		t.Error("wrapped ErrAuth should not match ErrRateLimit")
	}

	rl := fmt.Errorf("%w: too many requests", ErrRateLimit)
	if !IsRateLimited(rl) {
		t.Error("IsRateLimited(wrapped ErrRateLimit) = false")
	}

	pe := fmt.Errorf("%w: missing balance field", ErrParse)
	if !IsParseError(pe) {
		t.Error("IsParseError(wrapped ErrParse) = false")
	}

	ns := fmt.Errorf("%w: mimo has no usage api", ErrNotSupported)
	if !IsNotSupported(ns) {
		t.Error("IsNotSupported(wrapped ErrNotSupported) = false")
	}
}

func TestUnrelatedErrorNotMatched(t *testing.T) {
	plain := errors.New("network timeout")
	for name, fn := range map[string]func(error) bool{
		"auth":       IsAuthError,
		"ratelimit":  IsRateLimited,
		"parse":      IsParseError,
		"notsupport": IsNotSupported,
	} {
		if fn(plain) {
			t.Errorf("%s matched unrelated error", name)
		}
	}
}
