package orders

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

type CartService interface {
	GetCart(userID int) ([]models.CartItem, float64)
	ClearCart(userID int)
}

type Service struct {
	DB   *store.DB
	Cart CartService
}

func New(db *store.DB, cart CartService) *Service {
	return &Service{
		DB:   db,
		Cart: cart,
	}
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

const (
	freeDeliveryThreshold = 5000.0
	deliveryFee            = 300.0
)

func calculateTotals(subtotal float64) (float64, float64) {
	if subtotal >= freeDeliveryThreshold {
		return 0, subtotal
	}

	return deliveryFee, subtotal + deliveryFee
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/checkout", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)

		items, subtotal := s.Cart.GetCart(userID)
		delivery, total := calculateTotals(subtotal)

		if r.Method == http.MethodGet {
			token, err := csrf.IssueCookie(w, r)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title":      "Checkout",
				"Items":      items,
				"Subtotal":   subtotal,
				"Delivery":   delivery,
				"Total":     total,
				"CSRFToken":  token,
				"LoggedIn":   true,
				"UserName":  middleware.GetUserName(r),
				"UserRole":  middleware.GetUserRole(r),
			})

			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := csrf.Verify(r); err != nil {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		address := strings.TrimSpace(r.FormValue("address"))

		if address == "" || len(items) == 0 {
			token, err := csrf.IssueCookie(w, r)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title":      "Checkout",
				"Error":      "Address required and cart must not be empty",
				"Items":      items,
				"Subtotal":   subtotal,
				"Delivery":   delivery,
				"Total":     total,
				"CSRFToken":  token,
				"LoggedIn":   true,
				"UserName":  middleware.GetUserName(r),
				"UserRole":  middleware.GetUserRole(r),
			})

			return
		}

		var orderItems []models.OrderItem

		for _, ci := range items {
			orderItems = append(orderItems, models.OrderItem{
				ProductID: ci.ProductID,
				Quantity:  ci.Quantity,
				Price:     ci.Product.Price,
				Product:   ci.Product,
			})
		}

		order, err := s.DB.CreateOrder(models.Order{
			UserID:  userID,
			Total:   total,
			Status:  "pending",
			Address: address,
		}, orderItems)

		if err != nil {
			token, tokenErr := csrf.IssueCookie(w, r)
			if tokenErr != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title":      "Checkout",
				"Error":      err.Error(),
				"Items":      items,
				"Subtotal":   subtotal,
				"Delivery":   delivery,
				"Total":     total,
				"CSRFToken":  token,
				"LoggedIn":   true,
				"UserName":  middleware.GetUserName(r),
				"UserRole":  middleware.GetUserRole(r),
			})

			return
		}

		s.Cart.ClearCart(userID)

		http.Redirect(
			w,
			r,
			fmt.Sprintf("/orders/%d?success=1", order.ID),
			http.StatusFound,
		)
	}))

	mux.HandleFunc("/orders", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)

		orders := s.DB.GetOrders(userID)

		tmpl.ExecuteTemplate(w, "orders.html", map[string]interface{}{
			"Title":     "My Orders",
			"Orders":    orders,
			"LoggedIn":  true,
			"UserName": middleware.GetUserName(r),
			"UserRole": middleware.GetUserRole(r),
		})
	}))

	mux.HandleFunc("/orders/", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)

		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")

		if len(parts) == 0 {
			http.NotFound(w, r)
			return
		}

		orderID, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil || orderID <= 0 {
			http.NotFound(w, r)
			return
		}

		order, err := s.DB.GetOrder(orderID, userID)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		tmpl.ExecuteTemplate(w, "order_payment.html", map[string]interface{}{
			"Title":    fmt.Sprintf("Order #%d", order.ID),
			"Order":    order,
			"Success":  r.URL.Query().Get("success") == "1",
			"LoggedIn": true,
			"UserName": middleware.GetUserName(r),
			"UserRole": middleware.GetUserRole(r),
		})
	}))
}