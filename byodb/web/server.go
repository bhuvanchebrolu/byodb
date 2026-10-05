package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"byodb/ql"
	"byodb/relational"
)

//go:embed app.html
var appHTML []byte

type Server struct {
	db   *relational.DB
	path string
	port int
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
	Error    string     `json:"error,omitempty"`
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/tables", s.handleTables)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/stats", s.handleStats)

	addr := fmt.Sprintf(":%d", s.port)
	fmt.Printf("\n🚀 BYODB Web Workbench is running at http://localhost:%d\n", s.port)
	fmt.Printf("Press Ctrl+C to stop the web server.\n\n")

	return http.ListenAndServe(addr, mux)
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

	var tableDefs []*relational.TableDef
	var views []map[string]string
	err := s.db.View(func(tx *relational.DBTX) error {
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
	})

	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"tables": tableDefs, "views": views})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(QueryResponse{Error: "invalid JSON body"})
		return
	}

	rawQueries := strings.Split(req.Query, ";")
	var lastResp QueryResponse

	var tx relational.DBTX
	s.db.Begin(&tx)
	committedOrAborted := false

	defer func() {
		if !committedOrAborted {
			s.db.Commit(&tx)
		}
	}()

	for _, qStr := range rawQueries {
		qStr = strings.TrimSpace(qStr)
		if qStr == "" {
			continue
		}

		stmt, err := ql.Parse(qStr)
		if err != nil {
			s.db.Abort(&tx)
			committedOrAborted = true
			json.NewEncoder(w).Encode(QueryResponse{Error: err.Error()})
			return
		}

		switch stmt.(type) {
		case *ql.BeginStmt:
			continue
		case *ql.CommitStmt:
			err := s.db.Commit(&tx)
			if err != nil {
				committedOrAborted = true
				json.NewEncoder(w).Encode(QueryResponse{Error: err.Error()})
				return
			}
			s.db.Begin(&tx)
			continue
		case *ql.RollbackStmt:
			s.db.Abort(&tx)
			s.db.Begin(&tx)
			lastResp = QueryResponse{Affected: 0}
			continue
		}

		res, err := ql.Execute(&tx, stmt)
		if err != nil {
			s.db.Abort(&tx)
			committedOrAborted = true
			json.NewEncoder(w).Encode(QueryResponse{Error: err.Error()})
			return
		}

		var resp QueryResponse
		if res.Rows != nil {
			resp.Columns = res.Names
			for res.Rows.Valid() {
				var rec relational.Record
				if err := res.Rows.Deref(&rec); err != nil {
					s.db.Abort(&tx)
					committedOrAborted = true
					json.NewEncoder(w).Encode(QueryResponse{Error: err.Error()})
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
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(stats)
}
