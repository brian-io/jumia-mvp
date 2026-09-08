package catalog

import (
	"fmt"
	"jumia-mvp/shared/middleware"
	"jumia-mvp/shared/models"
	"jumia-mvp/shared/store"
	"net/http"
	"strconv"
	"strings"
)

type Service struct{ DB *store.DB }

func New(db *store.DB) *Service { return &Service{DB: db} }

func (s *Service) SeedSampleData() {
	if len(s.DB.GetProducts("", "")) > 0 { return }

	// Create seller
	sellerID := 1
	s.DB.CreateUser(models.User{Name: "Demo Seller", Email: "seller@jumia.ke", PasswordHash: hashSeller(), Role: "seller"})
	products := []models.Product{
		{Name: "Samsung Galaxy A54", Description: "6.4\" display, 128GB storage, 5000mAh battery", Price: 45000, Stock: 50, Category: "Electronics", ImageURL: "📱", SellerID: sellerID},
		{Name: "Nike Air Max 270", Description: "Comfortable running shoes with air cushioning", Price: 12500, Stock: 30, Category: "Fashion", ImageURL: "👟", SellerID: sellerID},
		{Name: "Blender Pro 2000W", Description: "High-power blender for smoothies and cooking", Price: 8500, Stock: 25, Category: "Home & Kitchen", ImageURL: "🫙", SellerID: sellerID},
		{Name: "The Lean Startup", Description: "Eric Ries - How entrepreneurs use innovation", Price: 1800, Stock: 100, Category: "Books", ImageURL: "📚", SellerID: sellerID},
		{Name: "Organic Honey 500g", Description: "Pure Kenyan honey from Mount Kenya region", Price: 650, Stock: 200, Category: "Groceries", ImageURL: "🍯", SellerID: sellerID},
		{Name: "Dell Inspiron 15", Description: "Intel i5, 8GB RAM, 256GB SSD laptop", Price: 85000, Stock: 15, Category: "Electronics", ImageURL: "💻", SellerID: sellerID},
		{Name: "Levi's 501 Jeans", Description: "Classic straight fit denim jeans", Price: 5500, Stock: 40, Category: "Fashion", ImageURL: "👖", SellerID: sellerID},
		{Name: "Instant Pot Duo", Description: "7-in-1 electric pressure cooker", Price: 15000, Stock: 20, Category: "Home & Kitchen", ImageURL: "🍲", SellerID: sellerID},
		{Name: "Wireless Earbuds Pro", Description: "Active noise cancellation, 30hr battery", Price: 9500, Stock: 60, Category: "Electronics", ImageURL: "🎧", SellerID: sellerID},
		{Name: "Avocado 1kg", Description: "Fresh Kenyan avocados, Grade A", Price: 200, Stock: 500, Category: "Groceries", ImageURL: "🥑", SellerID: sellerID},
		{Name: "Yoga Mat Premium", Description: "Non-slip, 6mm thick exercise mat", Price: 3200, Stock: 45, Category: "Sports", ImageURL: "🧘", SellerID: sellerID},
		{Name: "Coffee Maker Deluxe", Description: "12-cup programmable coffee maker", Price: 6800, Stock: 18, Category: "Home & Kitchen", ImageURL: "☕", SellerID: sellerID},
	}
	for _, p := range products { s.DB.CreateProduct(p) }
}

func hashSeller() string {
	// sha256("jumia_salt_2024_" + "test") = 475049209873a22e3ebc338d2aaa7a3b283c207c1d2c3f5957284ef735a2074b
	return "475049209873a22e3ebc338d2aaa7a3b283c207c1d2c3f5957284ef735a2074b"
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" { http.NotFound(w, r); return }
		category := r.URL.Query().Get("category")
		search := r.URL.Query().Get("q")
		products := s.DB.GetProducts(category, search)
		_, loggedIn := middleware.GetUserID(r)
		tmpl.ExecuteTemplate(w, "index.html", map[string]interface{}{
			"Title": "Jumia Kenya - Shop Online", "Products": products,
			"Categories": s.DB.GetCategories(), "Category": category, "Search": search,
			"LoggedIn": loggedIn, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	})

	mux.HandleFunc("/product/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 { http.NotFound(w, r); return }
		id, _ := strconv.Atoi(parts[2])
		product, err := s.DB.GetProduct(id)
		if err != nil { http.NotFound(w, r); return }
		_, loggedIn := middleware.GetUserID(r)
		tmpl.ExecuteTemplate(w, "product.html", map[string]interface{}{
			"Title": product.Name + " - Jumia", "Product": product,
			"LoggedIn": loggedIn, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	})

	mux.HandleFunc("/seller/products/new", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if middleware.GetUserRole(r) != "seller" { http.Redirect(w, r, "/", http.StatusFound); return }
		if r.Method == http.MethodGet {
			tmpl.ExecuteTemplate(w, "product_form.html", map[string]interface{}{
				"Title": "Add Product", "LoggedIn": true,
				"UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
			})
			return
		}
		r.ParseForm()
		price, _ := strconv.ParseFloat(r.FormValue("price"), 64)
		stock, _ := strconv.Atoi(r.FormValue("stock"))
		userID, _ := middleware.GetUserID(r)
		p := models.Product{
			Name: r.FormValue("name"), Description: r.FormValue("description"),
			Price: price, Stock: stock, Category: r.FormValue("category"),
			ImageURL: r.FormValue("image_url"), SellerID: userID,
		}
		if p.Name == "" || p.Price <= 0 {
			tmpl.ExecuteTemplate(w, "product_form.html", map[string]interface{}{
				"Title": "Add Product", "Error": "Name and price required",
				"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
			})
			return
		}
		s.DB.CreateProduct(p)
		http.Redirect(w, r, "/seller/dashboard", http.StatusFound)
	}))

	mux.HandleFunc("/seller/dashboard", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if middleware.GetUserRole(r) != "seller" { http.Redirect(w, r, "/", http.StatusFound); return }
		userID, _ := middleware.GetUserID(r)
		products := s.DB.GetProductsBySellerID(userID)
		tmpl.ExecuteTemplate(w, "seller_dashboard.html", map[string]interface{}{
			"Title": "Seller Dashboard", "Products": products,
			"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	}))

	mux.HandleFunc("/seller/products/delete/", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		id, _ := strconv.Atoi(parts[len(parts)-1])
		userID, _ := middleware.GetUserID(r)
		s.DB.DeleteProduct(id, userID)
		http.Redirect(w, r, "/seller/dashboard", http.StatusFound)
	}))
}

func init() { _ = fmt.Sprintf } // keep import
