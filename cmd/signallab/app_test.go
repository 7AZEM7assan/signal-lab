package main

import (
	"encoding/json"
	"strings"
	"testing"

	"signallab/internal/appctl"
)

func TestReadyMessageCarriesTheTokenSeparatelyFromTheLoggableFields(t *testing.T) {
	r := appctl.Ready{Addr: "127.0.0.1:51734", Port: 51734, Token: strings.Repeat("ab", 32)}
	b, err := json.Marshal(readyMessage(r, "/data"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "ready" || m["addr"] != "127.0.0.1:51734" || m["port"] != float64(51734) || m["url"] != "http://127.0.0.1:51734" || m["data_dir"] != "/data" {
		t.Fatalf("unexpected message: %s", b)
	}
	if m["token"] != r.Token {
		t.Fatalf("the launcher must receive the token on this channel: %s", b)
	}
	// url, addr and port are the fields that may be logged; none of them may contain the secret.
	for _, k := range []string{"url", "addr"} {
		if strings.Contains(m[k].(string), r.Token) || strings.Contains(m[k].(string), "token") {
			t.Fatalf("%s leaks the token: %v", k, m[k])
		}
	}
}
