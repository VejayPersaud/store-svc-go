package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Product struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceCents int    `json:"priceCents"`
	Stock      int    `json:"stock"`
	CreatedAt  string `json:"created_at"`
}

var db *pgxpool.Pool

func mustGetEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env: %s", k)
	}
	return v
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}




func main() {
	ctx := context.Background()

	// connect to Postgres
	url := mustGetEnv("DATABASE_URL")
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("db connect error: %v", err)
	}
	db = pool
	defer db.Close()

	// ensure table exists
	if err := initSchema(ctx); err != nil {
		log.Fatalf("init schema: %v", err)
	}

	// routes
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/products", productsHandler)        // GET, POST
	mux.HandleFunc("/products/", productItemHandler)    // DELETE by id: /products/<id>

	handler := withCORS(mux)

	// start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("store-svc listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func initSchema(ctx context.Context) error {
	_, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS products(
  id uuid PRIMARY KEY,
  name text NOT NULL,
  price_cents int NOT NULL,
  stock int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
`)
	return err
}

func productsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		getProducts(w, r)
	case http.MethodPost:
		createProduct(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func getProducts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := db.Query(ctx, `SELECT id, name, price_cents, stock, created_at FROM products ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	list := make([]Product, 0)

	for rows.Next() {
		var p Product
		var t time.Time // <-- scan timestamp here first
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceCents, &p.Stock, &t); err != nil {
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		p.CreatedAt = t.Format(time.RFC3339)
		list = append(list, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

type createBody struct {
	Name       string `json:"name"`
	PriceCents int    `json:"priceCents"`
	Stock      int    `json:"stock"`
}

func createProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body createBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.PriceCents <= 0 || body.Stock < 0 {
		http.Error(w, "invalid fields", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	createdAt := time.Now().UTC()

	_, err := db.Exec(ctx,
		`INSERT INTO products(id, name, price_cents, stock, created_at) VALUES($1,$2,$3,$4,$5)`,
		id, body.Name, body.PriceCents, body.Stock, createdAt)
	if err != nil {
		http.Error(w, "insert error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(Product{
		ID:         id,
		Name:       body.Name,
		PriceCents: body.PriceCents,
		Stock:      body.Stock,
		CreatedAt:  createdAt.Format(time.RFC3339),
	})


}


func productItemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// path looks like /products/<id>
	id := r.URL.Path[len("/products/"):]
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// try to delete, 204 even if not found (idempotent)
	_, err := db.Exec(r.Context(), `DELETE FROM products WHERE id = $1`, id)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}






