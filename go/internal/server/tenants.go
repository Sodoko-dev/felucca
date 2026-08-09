// Tenant administration endpoints (admin-key only; routing guards in
// serveAPI) and the quota gate used by sandbox create/fork.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// newSecret returns n cryptographically random bytes hex-encoded.
func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable for key issuance.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// hashSecret is THE secret-hashing scheme for stored credentials (API keys,
// join tokens): sha256, hex-encoded. Mint and lookup sites must all use this
// one helper so the scheme can never diverge between issue and verify.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mintKey creates an API key record for a tenant and returns (record, full
// secret). The secret is shown exactly once; only its sha256 is stored.
func mintKey(tenantID string, now int64) (*store.APIKey, string) {
	secret := "hearth_sk_" + newSecret(24)
	return &store.APIKey{
		ID:        "key-" + newSecret(6),
		TenantID:  tenantID,
		KeyHash:   hashSecret(secret),
		Prefix:    secret[:14], // "hearth_sk_" + 4 chars: enough to identify, useless to guess
		CreatedAt: now,
	}, secret
}

func (srv *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		MaxSandboxes int64  `json:"max_sandboxes"`
		MaxVcpus     int64  `json:"max_vcpus"`
		MaxMemMiB    int64  `json:"max_mem_mib"`
		MaxDiskGb    int64  `json:"max_disk_gb"`
		// Lifecycle defaults (v4 P5.2): seconds; 0 = no policy. Unlike the
		// per-sandbox overrides there is no -1 here — "off" IS the zero.
		DefaultIdleSleepS    int64 `json:"default_idle_sleep_s"`
		DefaultAsleepDeleteS int64 `json:"default_asleep_delete_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if req.Name == "" {
		writeJSON(w, 400, []byte(`{"error":"name required"}`))
		return
	}
	if len(req.Name) > 64 {
		writeJSON(w, 400, []byte(`{"error":"name too long (max 64)"}`))
		return
	}
	if req.DefaultIdleSleepS < 0 || (req.DefaultIdleSleepS > 0 && !validPolicy(req.DefaultIdleSleepS, minIdleSleepS)) ||
		req.DefaultAsleepDeleteS < 0 || (req.DefaultAsleepDeleteS > 0 && !validPolicy(req.DefaultAsleepDeleteS, minAsleepDeleteS)) {
		writeJSON(w, 400, []byte(`{"error":"bad lifecycle default: 0 or seconds within bounds"}`))
		return
	}
	now := time.Now().Unix()
	t := &store.Tenant{
		ID:                   "tn-" + newSecret(4),
		Name:                 req.Name,
		MaxSandboxes:         req.MaxSandboxes,
		MaxVcpus:             req.MaxVcpus,
		MaxMemMiB:            req.MaxMemMiB,
		MaxDiskGb:            req.MaxDiskGb,
		DefaultIdleSleepS:    req.DefaultIdleSleepS,
		DefaultAsleepDeleteS: req.DefaultAsleepDeleteS,
		CreatedAt:            now,
	}
	if err := srv.db.CreateTenant(t); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeJSON(w, 409, []byte(`{"error":"tenant name exists"}`))
			return
		}
		slog.Error("create tenant", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	key, secret := mintKey(t.ID, now)
	if err := srv.db.CreateKey(key); err != nil {
		slog.Error("create key", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	body, _ := json.Marshal(struct {
		Tenant *store.Tenant `json:"tenant"`
		APIKey string        `json:"api_key"`
		KeyID  string        `json:"key_id"`
	}{t, secret, key.ID})
	writeJSON(w, 201, body)
}

func (srv *Server) listTenants(w http.ResponseWriter) {
	ts, err := srv.db.ListTenants()
	if err != nil {
		slog.Error("list tenants", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if ts == nil {
		ts = []*store.Tenant{}
	}
	body, _ := json.Marshal(struct {
		Tenants []*store.Tenant `json:"tenants"`
	}{ts})
	writeJSON(w, 200, body)
}

func (srv *Server) createTenantKey(w http.ResponseWriter, tenantID string) {
	t, err := srv.db.GetTenant(tenantID)
	if err != nil {
		slog.Error("get tenant", "tenant", tenantID, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if t == nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	key, secret := mintKey(t.ID, time.Now().Unix())
	if err := srv.db.CreateKey(key); err != nil {
		slog.Error("create key", "tenant", tenantID, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	body, _ := json.Marshal(struct {
		APIKey string `json:"api_key"`
		KeyID  string `json:"key_id"`
	}{secret, key.ID})
	writeJSON(w, 201, body)
}

func (srv *Server) revokeTenantKey(w http.ResponseWriter, keyID string) {
	ok, err := srv.db.RevokeKey(keyID, time.Now().Unix())
	if err != nil {
		slog.Error("revoke key", "key", keyID, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if !ok {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	writeEmpty(w, 204)
}

// quotaExceeded checks whether adding one sandbox of the given shape would
// exceed the tenant's quotas. Returns "" when allowed, otherwise the name of
// the exceeded quota. Zero quota values mean unlimited. diskGB is the new
// sandbox's requested disk (0 = unresized base image); existing sandboxes
// count their EffectiveDiskGB so pre-P4 rows aren't free.
func (srv *Server) quotaExceeded(tenantID string, vcpus uint32, memMiB uint64, diskGB uint32) string {
	t, err := srv.db.GetTenant(tenantID)
	if err != nil {
		slog.Error("quota lookup", "tenant", tenantID, "err", err)
		return "" // store trouble must not block the data plane
	}
	if t == nil {
		return "" // unknown tenant row (e.g. deleted): nothing to enforce
	}

	if diskGB == 0 {
		diskGB = model.BaseImageDiskGB
	}
	var count int64
	var sumVcpus int64
	var sumMem int64
	var sumDisk int64
	srv.st.Lock()
	for _, sb := range srv.st.Sandboxes {
		if sb.TenantID != tenantID {
			continue
		}
		count++
		sumVcpus += int64(sb.VCPUs)
		sumMem += int64(sb.MemMiB)
		sumDisk += int64(sb.EffectiveDiskGB())
	}
	srv.st.Unlock()

	switch {
	case t.MaxSandboxes > 0 && count+1 > t.MaxSandboxes:
		return "sandboxes"
	case t.MaxVcpus > 0 && sumVcpus+int64(vcpus) > t.MaxVcpus:
		return "vcpus"
	case t.MaxMemMiB > 0 && sumMem+int64(memMiB) > t.MaxMemMiB:
		return "mem_mib"
	case t.MaxDiskGb > 0 && sumDisk+int64(diskGB) > t.MaxDiskGb:
		return "disk_gb"
	}
	return ""
}
