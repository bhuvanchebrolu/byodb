package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"byodb/ql"
	"byodb/relational"
)

//go:embed app.html
var appHTML []byte

type Server struct {
	db       *relational.DB
	path     string
	port     int
	activeTx *relational.DBTX
	inTx     bool
	txMutex  sync.Mutex
}

func NewServer(db *relational.DB, path string, port int) *Server {
	return &Server{
		db:   db,
		path: path,
		port: port,
	}
}

type QueryRequest struct {
	Query string `json:"query"`
}

type QueryResponse struct {
	Columns  []string   `json:"columns,omitempty"`
	Rows     [][]string `json:"rows,omitempty"`
	Affected int64      `json:"affected"`
	InTx     bool       `json:"in_tx"`
	Error    string     `json:"error,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/tables", s.handleTables)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/stats", s.handleStats)
	return mux
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.port)
	fmt.Printf("\n🚀 BYODB Web Workbench is running at http://localhost:%d\n", s.port)
	fmt.Printf("Press Ctrl+C to stop the web server.\n\n")

	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(appHTML)
}

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.txMutex.Lock()
	defer s.txMutex.Unlock()

	var tableDefs []*relational.TableDef
	var views []map[string]string

	fetchData := func(tx *relational.DBTX) error {
		names, err := tx.TableList()
		if err != nil {
			return err
		}
		for _, name := range names {
			def, err := tx.TableDef(name)
			if err == nil && def != nil {
				tableDefs = append(tableDefs, def)
			}
		}
		vNames, _ := tx.ViewList()
		for _, vn := range vNames {
			q, _ := tx.ViewGet(vn)
			views = append(views, map[string]string{"name": vn, "query": q})
		}
		return nil
	}

	var err error
	if s.inTx && s.activeTx != nil {
		err = fetchData(s.activeTx)
	} else {
		err = s.db.View(fetchData)
	}

	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"tables": tableDefs,
		"views":  views,
		"in_tx":  s.inTx,
	})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(QueryResponse{Error: "invalid JSON body", InTx: s.inTx})
		return
	}

	s.txMutex.Lock()
	defer s.txMutex.Unlock()

	rawQueries := strings.Split(req.Query, ";")
	var lastResp QueryResponse
	var localTx *relational.DBTX

	getTx := func() *relational.DBTX {
		if s.inTx && s.activeTx != nil {
			return s.activeTx
		}
		if localTx == nil {
			localTx = &relational.DBTX{}
			s.db.Begin(localTx)
		}
		return localTx
	}

	for _, qStr := range rawQueries {
		qStr = strings.TrimSpace(qStr)
		if qStr == "" {
			continue
		}

		stmt, err := ql.Parse(qStr)
		if err != nil {
			if s.inTx && s.activeTx != nil {
				s.db.Abort(s.activeTx)
				s.activeTx = nil
				s.inTx = false
			} else if localTx != nil {
				s.db.Abort(localTx)
				localTx = nil
			}
			json.NewEncoder(w).Encode(QueryResponse{Error: err.Error(), InTx: s.inTx})
			return
		}

		switch stmt.(type) {
		case *ql.BeginStmt:
			if !s.inTx {
				if localTx != nil {
					s.activeTx = localTx
					localTx = nil
				} else {
					s.activeTx = &relational.DBTX{}
					s.db.Begin(s.activeTx)
				}
				s.inTx = true
			}
			lastResp = QueryResponse{Affected: 0, InTx: s.inTx}
			continue

		case *ql.CommitStmt:
			if s.inTx && s.activeTx != nil {
				err := s.db.Commit(s.activeTx)
				s.activeTx = nil
				s.inTx = false
				if err != nil {
					json.NewEncoder(w).Encode(QueryResponse{Error: err.Error(), InTx: s.inTx})
					return
				}
			} else if localTx != nil {
				err := s.db.Commit(localTx)
				localTx = nil
				if err != nil {
					json.NewEncoder(w).Encode(QueryResponse{Error: err.Error(), InTx: s.inTx})
					return
				}
			}
			lastResp = QueryResponse{Affected: 0, InTx: s.inTx}
			continue

		case *ql.RollbackStmt:
			if s.inTx && s.activeTx != nil {
				s.db.Abort(s.activeTx)
				s.activeTx = nil
				s.inTx = false
			} else if localTx != nil {
				s.db.Abort(localTx)
				localTx = nil
			}
			lastResp = QueryResponse{Affected: 0, InTx: s.inTx}
			continue
		}

		currentTx := getTx()
		res, err := ql.Execute(currentTx, stmt)
		if err != nil {
			if s.inTx && s.activeTx != nil {
				s.db.Abort(s.activeTx)
				s.activeTx = nil
				s.inTx = false
			} else if localTx != nil {
				s.db.Abort(localTx)
				localTx = nil
			}
			json.NewEncoder(w).Encode(QueryResponse{Error: err.Error(), InTx: s.inTx})
			return
		}

		var resp QueryResponse
		resp.InTx = s.inTx
		if res.Rows != nil {
			resp.Columns = res.Names
			for res.Rows.Valid() {
				var rec relational.Record
				if err := res.Rows.Deref(&rec); err != nil {
					if s.inTx && s.activeTx != nil {
						s.db.Abort(s.activeTx)
						s.activeTx = nil
						s.inTx = false
					} else if localTx != nil {
						s.db.Abort(localTx)
						localTx = nil
					}
					json.NewEncoder(w).Encode(QueryResponse{Error: err.Error(), InTx: s.inTx})
					return
				}
				var rowStr []string
				for _, val := range rec.Vals {
					rowStr = append(rowStr, val.String())
				}
				resp.Rows = append(resp.Rows, rowStr)
				res.Rows.Next()
			}
		} else {
			resp.Affected = res.Affected
		}
		lastResp = resp
	}

	if !s.inTx && localTx != nil {
		s.db.Commit(localTx)
		localTx = nil
	}

	lastResp.InTx = s.inTx
	json.NewEncoder(w).Encode(lastResp)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fi, err := os.Stat(s.path)
	var size int64
	if err == nil {
		size = fi.Size()
	}

	pageCount := size / 4096

	stats := map[string]any{
		"path":          s.path,
		"fileSizeBytes": size,
		"pageCount":     pageCount,
		"pageSize":      4096,
		"in_tx":         s.inTx,
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(stats)
}

