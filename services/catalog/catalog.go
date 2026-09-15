package catalog

import (
	"agora/shared/csrf"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type Service struct{ DB *store.DB }

func New(db *store.DB) *Service { return &Service{DB: db} }

func (s *Service) SeedSampleData() {
	if len(s.DB.GetProducts("", "")) > 0 {
		return
	}

	// Create seller
	sellerID := 1
	s.DB.CreateUser(models.User{Name: "Demo Seller", Email: "seller@agora.ke", PasswordHash: hashSeller(), Role: "seller"})
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
	for _, p := range products {
		s.DB.CreateProduct(p)
	}
}

func hashSeller() string {
	// sha256("agora_salt_2024_" + "test") = 475049209873a22e3ebc338d2aaa7a3b283c207c1d2c3f5957284ef735a2074b
	return "475049209873a22e3ebc338d2aaa7a3b283c207c1d2c3f5957284ef735a2074b"
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		category := r.URL.Query().Get("category")
		search := r.URL.Query().Get("q")
		products := s.DB.GetProducts(category, search)

		_, loggedIn := middleware.GetUserID(r)

		tmpl.ExecuteTemplate(w, "index.html", map[string]interface{}{
			"Title":      "Agora Kenya - Shop Online",
			"Products":   products,
			"Categories": s.DB.GetCategories(),
			"Category":   category,
			"Search":     search,
			"LoggedIn":   loggedIn,
			"UserName":   middleware.GetUserName(r),
			"UserRole":   middleware.GetUserRole(r),
		})
	})

	mux.HandleFunc("/product/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")

		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}

		id, err := strconv.Atoi(parts[2])
		if err != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}

		product, err := s.DB.GetProduct(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		_, loggedIn := middleware.GetUserID(r)

		tmpl.ExecuteTemplate(w, "product.html", map[string]interface{}{
			"Title":    product.Name + " - Agora",
			"Product":  product,
			"LoggedIn": loggedIn,
			"UserName": middleware.GetUserName(r),
			"UserRole": middleware.GetUserRole(r),
		})
	})

	// ── Add product ──────────────────────────────────────

	mux.HandleFunc(
		"/seller/products/new",
		middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			if middleware.GetUserRole(r) != "seller" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}

			userID, ok := middleware.GetUserID(r)
			if !ok {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			if r.Method == http.MethodGet {
				token, err := csrf.IssueCookie(w, r)
				if err != nil {
					http.Error(w, "failed to initialize security token", http.StatusInternalServerError)
					return
				}

				tmpl.ExecuteTemplate(w, "product_form.html", map[string]interface{}{
					"Title":     "Add Product",
					"LoggedIn":  true,
					"UserName":  middleware.GetUserName(r),
					"UserRole":  middleware.GetUserRole(r),
					"CSRFToken": token,
				})
				return
			}

			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			if err := csrf.Verify(r); err != nil {
				tmpl.ExecuteTemplate(w, "product_form.html", map[string]interface{}{
					"Title":     "Add Product",
					"Error":     "invalid or expired security token",
					"LoggedIn":  true,
					"UserName":  middleware.GetUserName(r),
					"UserRole":  middleware.GetUserRole(r),
					"CSRFToken": "",
				})
				return
			}

			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form submission", http.StatusBadRequest)
				return
			}

			name := strings.TrimSpace(r.FormValue("name"))
			description := strings.TrimSpace(r.FormValue("description"))
			category := strings.TrimSpace(r.FormValue("category"))
			imageURL := strings.TrimSpace(r.FormValue("image_url"))

			price, err := strconv.ParseFloat(r.FormValue("price"), 64)
			if err != nil || price <= 0 {
				s.renderProductFormError(
					w,
					r,
					tmpl,
					"Enter a valid price.",
					name,
					description,
					category,
					imageURL,
				)
				return
			}

			stock, err := strconv.Atoi(r.FormValue("stock"))
			if err != nil || stock < 0 {
				s.renderProductFormError(
					w,
					r,
					tmpl,
					"Stock quantity cannot be negative.",
					name,
					description,
					category,
					imageURL,
				)
				return
			}

			if name == "" {
				s.renderProductFormError(
					w,
					r,
					tmpl,
					"Product name is required.",
					name,
					description,
					category,
					imageURL,
				)
				return
			}

			if len(name) > 200 {
				s.renderProductFormError(
					w,
					r,
					tmpl,
					"Product name is too long.",
					name,
					description,
					category,
					imageURL,
				)
				return
			}

			product := models.Product{
				Name:        name,
				Description: description,
				Price:       price,
				Stock:       stock,
				Category:    category,
				ImageURL:    imageURL,
				SellerID:    userID,
			}

			if _, err := s.DB.CreateProduct(product); err != nil {
				s.renderProductFormError(
					w,
					r,
					tmpl,
					"Failed to create product.",
					name,
					description,
					category,
					imageURL,
				)
				return
			}

			http.Redirect(w, r, "/seller/dashboard", http.StatusFound)
		}),
	)

	// ── Seller dashboard ─────────────────────────────────

	mux.HandleFunc(
		"/seller/dashboard",
		middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			if middleware.GetUserRole(r) != "seller" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}

			userID, ok := middleware.GetUserID(r)
			if !ok {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			token, err := csrf.IssueCookie(w, r)
			if err != nil {
				http.Error(w, "failed to initialize security token", http.StatusInternalServerError)
				return
			}

			products := s.DB.GetProductsBySellerID(userID)

			tmpl.ExecuteTemplate(w, "seller_dashboard.html", map[string]interface{}{
				"Title":     "Seller Dashboard",
				"Products":  products,
				"LoggedIn":  true,
				"UserName":  middleware.GetUserName(r),
				"UserRole":  middleware.GetUserRole(r),
				"CSRFToken": token,
			})
		}),
	)

	// ── Stock increment/decrement ────────────────────────

	mux.HandleFunc(
		"/seller/products/stock/",
		middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			if middleware.GetUserRole(r) != "seller" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			if err := csrf.Verify(r); err != nil {
				http.Error(w, "invalid or expired security token", http.StatusForbidden)
				return
			}

			userID, ok := middleware.GetUserID(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")

			if len(parts) < 5 {
				http.NotFound(w, r)
				return
			}

			productID, err := strconv.Atoi(parts[4])
			if err != nil || productID <= 0 {
				http.NotFound(w, r)
				return
			}

			delta, err := strconv.Atoi(r.FormValue("delta"))
			if err != nil || (delta != 1 && delta != -1) {
				http.Error(w, "invalid stock adjustment", http.StatusBadRequest)
				return
			}

			if _, err := s.DB.UpdateProductStock(productID, userID, delta); err != nil {
				http.Redirect(
					w,
					r,
					"/seller/dashboard?error="+urlQueryEscape(err.Error()),
					http.StatusFound,
				)
				return
			}

			http.Redirect(w, r, "/seller/dashboard", http.StatusFound)
		}),
	)

	// ── Delete product ───────────────────────────────────

	mux.HandleFunc(
		"/seller/products/delete/",
		middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			if middleware.GetUserRole(r) != "seller" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			if err := csrf.Verify(r); err != nil {
				http.Error(w, "invalid or expired security token", http.StatusForbidden)
				return
			}

			userID, ok := middleware.GetUserID(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")

			if len(parts) < 5 {
				http.NotFound(w, r)
				return
			}

			id, err := strconv.Atoi(parts[4])
			if err != nil || id <= 0 {
				http.NotFound(w, r)
				return
			}

			s.DB.DeleteProduct(id, userID)

			http.Redirect(w, r, "/seller/dashboard", http.StatusFound)
		}),
	)
}

func init() { _ = fmt.Sprintf } // keep import
