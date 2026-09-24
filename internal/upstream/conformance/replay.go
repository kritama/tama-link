package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
)

// captured is one request the fixture server observed.
type captured struct {
	Header http.Header
	Body   []byte
}

// replay serves one fixture's expected response and records the request.
type replay struct {
	server   *httptest.Server
	requests []captured
}

func newReplay(fx Fixture) *replay {
	r := &replay{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		r.requests = append(r.requests, captured{Header: req.Header.Clone(), Body: body})
		writeExpected(w, fx, requestID(body))
	}))
	return r
}

func (r *replay) close() {
	r.server.Close()
}

func (r *replay) url() string { return r.server.URL }

func writeExpected(w http.ResponseWriter, fx Fixture, clientID string) {
	for name, value := range fx.Expected.Headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(fx.Expected.Status)
	if len(fx.Expected.Events) > 0 {
		for _, event := range fx.Expected.Events {
			_, _ = w.Write(sseData(rewriteID(event, fx.RequestID(), clientID)))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		return
	}
	_, _ = w.Write(rewriteID(fx.Expected.Body, fx.RequestID(), clientID))
}

func requestID(body []byte) string {
	var view struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &view) != nil {
		return ""
	}
	return view.ID
}
