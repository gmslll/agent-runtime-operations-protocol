package event

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

type SecureTokenSource struct{}

func (SecureTokenSource) NewEventToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	allZero := true
	for _, item := range value {
		allZero = allZero && item == 0
	}
	if allZero {
		return "", errors.New("generated an all-zero event token")
	}
	return "evtcap_" + base64.RawURLEncoding.EncodeToString(value), nil
}
