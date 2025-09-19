package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Product struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceCents int    `json:"priceCents"`
	Stock      int    `json:"stock"`
	CreatedAt  string `json:"created_at"`
}

var (
	db  *pgxpool.Pool
	rdb *redis.Client // nil if REDIS_URL not set
)

// --- helpers ---

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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- main ---

func main() {
	ctx := context.Background()

	// Postgres
	pool, err := pgxpool.New(ctx, mustGetEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect error: %v", err)
	}
	db = pool
	defer db.Close()

	// Ensure schema
	if err := initSchema(ctx); err != nil {
		log.Fatalf("init schema: %v", err)
	}

	// Redis (optional)
	if ru := os.Getenv("REDIS_URL"); ru != "" {
		opt, err := redis.ParseURL(ru) // handles redis:// and rediss://
		if err != nil {
			log.Fatalf("redis parse error: %v", err)
		}
		rdb = redis.NewClient(opt)
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Fatalf("redis ping error: %v", err)
		}
		log.Println("redis connected")
	} else {
		log.Println("redis disabled (REDIS_URL not set)")
	}

	// Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/products", productsHandler)     // GET, POST
	mux.HandleFunc("/products/", productItemHandler) // DELETE /products/:id

	handler := withCORS(mux)

	// Serve
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("store-svc listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}

// --- schema ---

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

// --- handlers ---

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
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

func productItemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/products/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// validate UUID
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid id (must be UUID)", http.StatusBadRequest)
		return
	}
	// delete (idempotent)
	if _, err := db.Exec(r.Context(), `DELETE FROM products WHERE id = $1::uuid`, id); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	// invalidate cache
	if rdb != nil {
		_ = rdb.Del(r.Context(), "products:all").Err()
	}
	w.WriteHeader(http.StatusNoContent)
}

func getProducts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1) try cache
	if rdb != nil {
		if s, err := rdb.Get(ctx, "products:all").Result(); err == nil && s != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(s))
			return
		}
	}

	// 2) query DB
	rows, err := db.Query(ctx, `SELECT id, name, price_cents, stock, created_at FROM products ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	list := make([]Product, 0)
	for rows.Next() {
		var p Product
		var t time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceCents, &p.Stock, &t); err != nil {
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		p.CreatedAt = t.Format(time.RFC3339)
		list = append(list, p)
	}

	// 3) write response + populate cache
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(list)
	w.Write(b)
	if rdb != nil {
		_ = rdb.Set(ctx, "products:all", b, 30*time.Second).Err()
	}
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

	if _, err := db.Exec(ctx,
		`INSERT INTO products(id, name, price_cents, stock, created_at) VALUES($1,$2,$3,$4,$5)`,
		id, body.Name, body.PriceCents, body.Stock, createdAt,
	); err != nil {
		http.Error(w, "insert error", http.StatusInternalServerError)
		return
	}

	// invalidate cache
	if rdb != nil {
		_ = rdb.Del(ctx, "products:all").Err()
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
