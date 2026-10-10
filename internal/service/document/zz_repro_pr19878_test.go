package document

// Reproductions for the review of PR #19878. Not part of the PR.
// Each test asserts the CORRECT behaviour: a failure means the finding is confirmed.
// Run with REPRO_ES_HOST=http://127.0.0.1:19200.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/engine/elasticsearch"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/server/config"
	"ragflow/internal/service"
)

type reproEnv struct {
	es     *elasticsearch.Engine
	svc    *DocumentService
	meta   *service.MetadataService
	tenant string
	kb     string
}

func newReproEnv(t *testing.T, host string) *reproEnv {
	t.Helper()
	esHost := os.Getenv("REPRO_ES_HOST")
	if esHost == "" {
		t.Skip("set REPRO_ES_HOST")
	}
	if host == "" {
		host = esHost
	}
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)

	name := strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	tenant := fmt.Sprintf("t_%s_%d", name, time.Now().UnixNano())
	kb := "kb_" + name
	status := string(entity.StatusValid)
	if err := db.Create(&entity.Knowledgebase{ID: kb, TenantID: tenant, Name: kb, EmbdID: "e", Permission: "me", CreatedBy: "u", Status: &status}).Error; err != nil {
		t.Fatalf("create kb: %v", err)
	}

	es, err := elasticsearch.NewEngine(t.Context(), config.ElasticsearchConfig{Hosts: host})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := es.CreateMetadataStore(t.Context(), tenant); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, esHost+"/ragflow_doc_meta_"+tenant, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	meta := service.NewMetadataServiceForTest(dao.NewKnowledgebaseDAO(), es)
	svc := testDocumentService(t)
	svc.metadataSvc = meta
	return &reproEnv{es: es, svc: svc, meta: meta, tenant: tenant, kb: kb}
}

func (e *reproEnv) insert(t *testing.T, docs map[string]map[string]interface{}) []string {
	t.Helper()
	rows := make([]map[string]interface{}, 0, len(docs))
	ids := make([]string, 0, len(docs))
	for id, fields := range docs {
		rows = append(rows, map[string]interface{}{"id": id, "kb_id": e.kb, "meta_fields": fields})
		ids = append(ids, id)
	}
	for start := 0; start < len(rows); start += 5000 {
		if _, err := e.es.InsertMetadata(t.Context(), rows[start:min(start+5000, len(rows))], e.tenant); err != nil {
			t.Fatalf("InsertMetadata: %v", err)
		}
	}
	return ids
}

// Finding 1: the value the menu offers must be a value the list filter can resolve.
func TestRepro1_FacetLabelsMatchListFilter(t *testing.T) {
	env := newReproEnv(t, "")
	ids := env.insert(t, map[string]map[string]interface{}{
		"d1": {"price": 1234567},
		"d2": {"score": 0.1},
		"d3": {"ts": "2026-07-23T10:00:00+02:00"},
		"d4": {"author": "Mario"},
	})
	counts, _, ok := env.meta.GetMetadataFacets(t.Context(), env.kb, ids)
	if !ok {
		t.Fatalf("fast path not taken")
	}
	listSide, err := env.svc.GetMetadataByKBs(t.Context(), []string{env.kb})
	if err != nil {
		t.Fatalf("GetMetadataByKBs: %v", err)
	}
	for key, values := range counts {
		raw, _ := listSide[key].(map[string][]string)
		for value, n := range values {
			// applyDocumentMetadataFilter resolves a selection as rawValues[value].
			matched := len(raw[value])
			known := make([]string, 0, len(raw))
			for k := range raw {
				known = append(known, k)
			}
			t.Logf("key=%s menu=%q count=%d | list filter knows %q -> matches %d", key, value, n, known, matched)
			if int64(matched) != n {
				t.Errorf("CONFIRMED: %s=%q shows %d in the menu but the list filter matches %d", key, value, n, matched)
			}
		}
	}
}

// Finding 2: one uncoverable value must not disable the fast paths for the whole dataset.
func TestRepro2_OneBadValueDisablesDataset(t *testing.T) {
	cases := map[string]interface{}{
		"long_string": strings.Repeat("x", 300),
		"dict_value":  map[string]interface{}{"a": 1},
		"blank_value": "",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			env := newReproEnv(t, "")
			docs := map[string]map[string]interface{}{}
			for i := 0; i < 50; i++ {
				docs[fmt.Sprintf("d%02d", i)] = map[string]interface{}{"author": fmt.Sprintf("a%d", i%5)}
			}
			docs["bad"] = map[string]interface{}{"author": "a0", "extra": bad}
			ids := env.insert(t, docs)

			_, _, facetOK := env.meta.GetMetadataFacets(t.Context(), env.kb, ids)
			_, vsErr := env.es.MetaValueSpace(t.Context(), env.tenant, []string{env.kb})
			t.Logf("facet fast path ok=%v; value space err=%v", facetOK, vsErr)
			if !facetOK {
				t.Errorf("CONFIRMED: facet falls back to the scan for the whole dataset")
			}
			if errors.Is(vsErr, types.ErrMetaValueSpaceIncomplete) {
				t.Errorf("CONFIRMED: value space refused for the whole dataset (gen_meta_filter skips filtering, author is fully covered)")
			}
		})
	}
}

// Finding 6: menu counts and list filtering agree past 10,000 metadata rows.
func TestRepro6_TenThousandCap(t *testing.T) {
	env := newReproEnv(t, "")
	docs := make(map[string]map[string]interface{}, 15000)
	for i := 0; i < 15000; i++ {
		docs[fmt.Sprintf("doc%05d", i)] = map[string]interface{}{"author": "X"}
	}
	ids := env.insert(t, docs)
	counts, carrying, ok := env.meta.GetMetadataFacets(t.Context(), env.kb, ids)
	if !ok {
		t.Fatalf("fast path not taken")
	}
	listSide, err := env.svc.GetMetadataByKBs(t.Context(), []string{env.kb})
	if err != nil {
		t.Fatalf("GetMetadataByKBs: %v", err)
	}
	raw, _ := listSide["author"].(map[string][]string)
	t.Logf("menu author=X: %d, carrying=%d, empty_metadata=%d | list filter author=X matches %d",
		counts["author"]["X"], carrying, int64(len(ids))-carrying, len(raw["X"]))
	if int64(len(raw["X"])) != counts["author"]["X"] {
		t.Errorf("CONFIRMED: menu says %d, selecting it lists %d", counts["author"]["X"], len(raw["X"]))
	}
}

// Finding 7: the first round must fit search.max_buckets.
func TestRepro7_BucketBudget(t *testing.T) {
	env := newReproEnv(t, "")
	const keys = 70
	pageSize := 65536 / keys
	docs := make(map[string]map[string]interface{}, pageSize)
	for d := 0; d < pageSize; d++ {
		fields := make(map[string]interface{}, keys)
		for k := 0; k < keys; k++ {
			fields[fmt.Sprintf("k%02d", k)] = fmt.Sprintf("v%d", d)
		}
		docs[fmt.Sprintf("doc%04d", d)] = fields
	}
	ids := env.insert(t, docs)
	_, vsErr := env.es.MetaValueSpace(t.Context(), env.tenant, []string{env.kb})
	_, _, err := env.es.MetaFacets(t.Context(), env.tenant, env.kb, ids)
	t.Logf("value space err=%v", vsErr)
	t.Logf("facets err=%v", err)
	if vsErr != nil || err != nil {
		t.Errorf("CONFIRMED: request rejected with %d keys x %d values", keys, pageSize)
	}
}

// Finding 8: request body size for one popover open on a 100k-document dataset.
func TestRepro8_RequestSize(t *testing.T) {
	target, _ := url.Parse(os.Getenv("REPRO_ES_HOST"))
	var searchBytes, searches atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_search") {
			body, _ := io.ReadAll(r.Body)
			searchBytes.Add(int64(len(body)))
			searches.Add(1)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	env := newReproEnv(t, srv.URL)
	docs := map[string]map[string]interface{}{}
	for i := 0; i < 100; i++ {
		docs[fmt.Sprintf("%032x", i)] = map[string]interface{}{"author": fmt.Sprintf("a%d", i%7)}
	}
	env.insert(t, docs)
	ids := make([]string, 100000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i)
	}
	searchBytes.Store(0)
	searches.Store(0)
	if _, _, err := env.es.MetaFacets(t.Context(), env.tenant, env.kb, ids); err != nil {
		t.Fatalf("MetaFacets: %v", err)
	}
	t.Logf("MEASURED: %d _search requests, %.2f MB of request body for 100k document ids",
		searches.Load(), float64(searchBytes.Load())/1e6)
}

// Finding 5: a cancelled request must not trigger the document scan.
type reproFacetEngine struct {
	engine.DocEngine
	facetErr     error
	scanRequests atomic.Int64
}

func (f *reproFacetEngine) MetaFacets(ctx context.Context, tenantID, kbID string, docIDs []string) (map[string]map[string]int64, int64, error) {
	return nil, 0, f.facetErr
}

func (f *reproFacetEngine) SearchMetadata(ctx context.Context, req *types.SearchMetadataRequest) (*types.SearchMetadataResult, error) {
	f.scanRequests.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &types.SearchMetadataResult{}, nil
}

func TestRepro5_AnyErrorFallsBackToScan(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	status := string(entity.StatusValid)
	if err := db.Create(&entity.Knowledgebase{ID: "kb5", TenantID: "t5", Name: "kb5", EmbdID: "e", Permission: "me", CreatedBy: "u", Status: &status}).Error; err != nil {
		t.Fatalf("create kb: %v", err)
	}
	for name, mk := range map[string]func() (context.Context, error){
		"context_canceled": func() (context.Context, error) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, context.Canceled
		},
		"transport_error": func() (context.Context, error) {
			return context.Background(), errors.New("dial tcp 127.0.0.1:9200: connect: connection refused")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, facetErr := mk()
			fake := &reproFacetEngine{facetErr: facetErr}
			svc := testDocumentService(t)
			svc.metadataSvc = service.NewMetadataServiceForTest(dao.NewKnowledgebaseDAO(), fake)
			_, err := svc.getDocumentMetadataFilter(ctx, "kb5", []string{"d1"})
			t.Logf("facet error %q -> scan requests issued: %d, final err: %v", facetErr, fake.scanRequests.Load(), err)
			if fake.scanRequests.Load() > 0 {
				t.Errorf("CONFIRMED: a non-coverage error still ran the document scan")
			}
		})
	}
}
