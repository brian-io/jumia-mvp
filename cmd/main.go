package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"

	authsvc "jumia-mvp/services/auth"
	catalogsvc "jumia-mvp/services/catalog"
	cartsvc "jumia-mvp/services/cart"
	orderssvc "jumia-mvp/services/orders"
	"jumia-mvp/shared/middleware"
	"jumia-mvp/shared/models"
	"jumia-mvp/shared/store"
)

type TemplateEngine struct{ t *template.Template }

func (te *TemplateEngine) ExecuteTemplate(w http.ResponseWriter, name string, data interface{}) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := te.t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("Template error [%s]: %v", name, err)
	}
	return nil
}

func hashPassword(password string) string {
	h := sha256.New()
	h.Write([]byte("jumia_salt_2024_" + password))
	return hex.EncodeToString(h.Sum(nil))
}

func buildTemplates() *TemplateEngine {
	funcMap := template.FuncMap{
		"mul":     func(a, b float64) float64 { return a * b },
		"add":     func(a, b float64) float64 { return a + b },
		"toFloat": func(i int) float64 { return float64(i) },
		"not": func(v interface{}) bool {
			if v == nil { return true }
			if b, ok := v.(bool); ok { return !b }
			if s, ok := v.(string); ok { return s == "" }
			return false
		},
		"countInStock": func(products []models.Product) int {
			count := 0
			for _, p := range products { if p.Stock > 0 { count++ } }
			return count
		},
		"totalValue": func(products []models.Product) string {
			total := 0.0
			for _, p := range products { total += p.Price * float64(p.Stock) }
			return strconv.FormatFloat(total, 'f', 0, 64)
		},
	}
	t := template.New("").Funcs(funcMap)
	t, err := t.ParseGlob("web/templates/*.html")
	if err != nil { log.Fatalf("Template parse error: %v", err) }
	return &TemplateEngine{t: t}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "8080" }

	db := store.Get()

	tmpl := buildTemplates()

	authService := authsvc.New(db)
	catalogService := catalogsvc.New(db)
	cartService := cartsvc.New(db)
	ordersService := orderssvc.New(db, cartService)

	catalogService.SeedSampleData()
	// Seed demo buyer
	db.CreateUser(models.User{
		Name: "Demo Buyer", Email: "buyer@demo.ke",
		PasswordHash: hashPassword("demo123"), Role: "buyer",
	})

	mux := http.NewServeMux()
	authService.RegisterRoutes(mux, tmpl)
	catalogService.RegisterRoutes(mux, tmpl)
	cartService.RegisterRoutes(mux, tmpl)
	ordersService.RegisterRoutes(mux, tmpl)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"jumia-mvp","version":"1.0.0"}`)
	})

	handler := middleware.Auth(db)(mux)
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		middleware.Auth(db)(mux).ServeHTTP(w, r)
	})

	log.Printf("🚀 Jumia MVP running at http://localhost:%s", port)
	log.Printf("   Demo buyer:  buyer@demo.ke / demo123")
	log.Printf("   Demo seller: seller@jumia.ke / test")
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
