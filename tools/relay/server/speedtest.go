package server

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

const maxSpeedtestMB = 1024
const speedtestChunkSize = 65536 // 64KB

func (s *Server) handleSpeedtest(w http.ResponseWriter, r *http.Request) {
	mbStr := mux.Vars(r)["mb"]
	mb, err := strconv.Atoi(mbStr)
	if err != nil || mb <= 0 || mb > maxSpeedtestMB {
		http.Error(w, fmt.Sprintf("Usage: /speedtest/<1-%d> (MB)", maxSpeedtestMB), http.StatusBadRequest)
		return
	}

	// passthrough: if > 0, proxy to upstream with passthrough-1
	if pt := r.URL.Query().Get("passthrough"); pt != "" {
		n, err := strconv.Atoi(pt)
		if err == nil && n > 0 {
			s.proxySpeedtest(w, r, mb, n-1)
			return
		}
	}

	totalBytes := int64(mb) * 1024 * 1024

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(totalBytes, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	buf := make([]byte, speedtestChunkSize)
	remaining := totalBytes
	for remaining > 0 {
		n := int64(speedtestChunkSize)
		if n > remaining {
			n = remaining
		}
		if _, err := io.ReadFull(rand.Reader, buf[:n]); err != nil {
			return
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		remaining -= n
	}
}

func (s *Server) proxySpeedtest(w http.ResponseWriter, r *http.Request, mb, passthrough int) {
	target := *s.upstream
	target.Path = fmt.Sprintf("/speedtest/%d", mb)
	target.RawQuery = fmt.Sprintf("passthrough=%d", passthrough)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("Host", s.upstream.Host)

	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
