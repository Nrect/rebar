package idem_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

const (
	opCreate = idem.Operation("orders.create")
	opUpdate = idem.Operation("orders.update")
)

func testConfig() idem.Config {
	return idem.Config{
		Operations:       []idem.Operation{opCreate, opUpdate},
		Retention:        idem.MinRetention,
		MaxResponseBytes: 64,
	}
}

func mustKey(t *testing.T, raw string) idem.Key {
	t.Helper()
	key, err := idem.ParseKey(raw)
	require.NoError(t, err)
	return key
}

// validRequest — запрос, который Config.CheckRequest пропускает.
func validRequest(t *testing.T) idem.Request {
	t.Helper()
	return idem.Request{
		Scope:     idem.Scope{Realm: "customers", Subject: "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"},
		Operation: opCreate, Key: mustKey(t, "k-1"),
		Method: http.MethodPost, Path: "/orders", RawQuery: "source=web", Body: []byte(`{"product":"a"}`),
	}
}
