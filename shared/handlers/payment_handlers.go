// This file is illustrative wiring, not a drop-in package — adapt the
// import paths, router (net/http vs chi/gin/etc.), and session lookup to
// match how the rest of your app is structured. It shows the checkout
// and M-Pesa endpoints and how they call into the refactored store package.
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"agora/mpesa"
	"agora/shared/csrf"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"

	"github.com/google/uuid"
)

var mpesaClient = mpesa.NewClientFromEnv()

// kenyanPhoneRE matches the normalized "2547XXXXXXXX" / "2541XXXXXXXX"
// form. Raw user input ("0712345678", "+254712345678", "254712345678")
// is normalized to this form by normalizeKenyanPhone before validation —
// the original code validated in this format but never normalized into
// it, so any input using the "0..." form the checkout page itself
// collects would always fail validation.
var kenyanPhoneRE = regexp.MustCompile(`^254[17]\d{8}$`)

// deliveryFeeFor mirrors the free-delivery-over-5000 rule shown in
// cart.html, kept in one place so the cart page, checkout page, and the
// order total stored in the database can't drift out of sync with each
// other.
func deliveryFeeFor(subtotal float64) float64 {
	if subtotal >= 5000 {
		return 0
	}
	return 300
}

// normalizeKenyanPhone accepts "0712345678", "+254712345678", or
// "254712345678" and returns "254712345678", or "" if the input doesn't
// look like a Kenyan Safaricom/Airtel number.
func normalizeKenyanPhone(raw string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, raw)

	switch {
	case strings.HasPrefix(digits, "254") && len(digits) == 12:
		// already normalized
	case strings.HasPrefix(digits, "0") && len(digits) == 10:
		digits = "254" + digits[1:]
	case len(digits) == 9:
		digits = "254" + digits
	default:
		return ""
	}

	if !kenyanPhoneRE.MatchString(digits) {
		return ""
	}
	return digits
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// ── Checkout ──────────────────────────────────────────

// CheckoutPageHandler renders the checkout form (GET /checkout). It issues
// both a CSRF token and a fresh idempotency key; the key is carried in a
// hidden form field and echoed back on submit so a double-click or a
// retried request after a dropped response can't create two orders — see
// store.CreateOrder's idempotencyKey parameter.
func CheckoutPageHandler(tmpl Tmpl) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.GetUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		items := store.Get().GetCartItems(userID)
		if len(items) == 0 {
			http.Redirect(w, r, "/cart", http.StatusFound)
			return
		}

		var subtotal float64
		for _, it := range items {
			subtotal += it.Product.Price * float64(it.Quantity)
		}

		token, err := csrf.IssueCookie(w, r)
		if err != nil {
			http.Error(w, "failed to initialize security token", http.StatusInternalServerError)
			return
		}

		tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
			"Title":          "Checkout",
			"Items":          items,
			"Total":          subtotal,
			"IdempotencyKey": uuid.NewString(),
			"CSRFToken":      token,
			"LoggedIn":       true,
			"UserName":       middleware.GetUserName(r),
			"UserRole":       middleware.GetUserRole(r),
		})
	}
}

// CheckoutSubmitHandler creates the order from the user's current cart
// (POST /checkout). Note that it does NOT send cart prices to
// store.CreateOrder — only product ids and quantities. CreateOrder looks
// up authoritative prices itself under row locks, so a stale cart price
// (or a tampered request) can never under/overcharge a customer.
func CheckoutSubmitHandler(tmpl Tmpl) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		userID, ok := middleware.GetUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		if err := csrf.Verify(r); err != nil {
			http.Redirect(w, r, "/checkout", http.StatusFound)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form submission", http.StatusBadRequest)
			return
		}

		address := strings.TrimSpace(r.FormValue("address"))
		paymentMethod := r.FormValue("payment_method")
		idempotencyKey := strings.TrimSpace(r.FormValue("idempotency_key"))

		cartItems := store.Get().GetCartItems(userID)
		if len(cartItems) == 0 {
			http.Redirect(w, r, "/cart", http.StatusFound)
			return
		}

		var subtotal float64
		orderItems := make([]models.OrderItem, 0, len(cartItems))
		for _, ci := range cartItems {
			subtotal += ci.Product.Price * float64(ci.Quantity)
			orderItems = append(orderItems, models.OrderItem{ProductID: ci.ProductID, Quantity: ci.Quantity})
		}

		order, err := store.Get().CreateOrder(models.Order{
			UserID:      userID,
			Address:     address,
			DeliveryFee: deliveryFeeFor(subtotal),
		}, orderItems, idempotencyKey)
		if err != nil {
			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title":    "Checkout",
				"Error":    friendlyCheckoutError(err),
				"Items":    cartItems,
				"Total":    subtotal,
				"Address":  address,
				"LoggedIn": true,
				"UserName": middleware.GetUserName(r),
				"UserRole": middleware.GetUserRole(r),
			})
			return
		}

		// The order now exists and stock has been decremented for it.
		// Clearing the cart is safe even if CreateOrder was a no-op replay
		// of an idempotency key — either way the items have been "spent".
		store.Get().ClearCart(userID)

		if paymentMethod == "cod" {
			store.Get().UpdateOrderStatus(order.ID, "cash_on_delivery")
			http.Redirect(w, r, fmt.Sprintf("/orders/%d", order.ID), http.StatusFound)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/orders/%d/pay", order.ID), http.StatusFound)
	}
}

// friendlyCheckoutError unwraps the sentinel errors store.CreateOrder
// wraps with product-specific context (via %w) and returns copy safe to
// show a customer. Must use errors.Is, not ==, since CreateOrder always
// wraps these with fmt.Errorf("product %d (%s): %w", ...).
func friendlyCheckoutError(err error) string {
	switch {
	case errors.Is(err, store.ErrInsufficientStock):
		return "One of the items in your cart no longer has enough stock. Please review your cart."
	case errors.Is(err, store.ErrProductInactive):
		return "One of the items in your cart is no longer available. Please review your cart."
	default:
		return "We couldn't place your order. Please try again."
	}
}

// CancelOrderHandler lets a customer cancel a still-pending order and get
// its reserved stock released back to the seller (POST /orders/{id}/cancel).
func CancelOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID, ok := middleware.GetUserID(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "please log in again")
		return
	}
	if err := csrf.Verify(r); err != nil {
		writeJSONError(w, http.StatusForbidden, "your session expired, please reload the page")
		return
	}
	orderID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid order id")
		return
	}
	if err := store.Get().CancelOrder(orderID, userID); err != nil {
		writeJSONError(w, http.StatusConflict, "this order can no longer be cancelled")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/orders/%d", orderID), http.StatusFound)
}

// ── M-Pesa payment ────────────────────────────────────

// OrderPaymentPageHandler renders the M-Pesa payment page (GET /orders/{id}/pay).
func OrderPaymentPageHandler(tmpl Tmpl) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.GetUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		orderID, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid order id", http.StatusBadRequest)
			return
		}
		order, err := store.Get().GetOrder(orderID, userID)
		if err != nil {
			http.Error(w, "order not found", http.StatusNotFound)
			return
		}
		if order.Status != "pending" {
			http.Redirect(w, r, fmt.Sprintf("/orders/%d", order.ID), http.StatusFound)
			return
		}

		token, err := csrf.IssueCookie(w, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		tmpl.ExecuteTemplate(w, "order_payment.html", map[string]interface{}{
			"Title":     "Pay for your order",
			"Order":     order,
			"CSRFToken": token,
			"LoggedIn":  true,
			"UserName":  middleware.GetUserName(r),
			"UserRole":  middleware.GetUserRole(r),
		})
	}
}

// InitiatePaymentHandler starts an STK push for an existing order
// (POST /orders/{id}/pay). Always responds JSON.
//
// The duplicate-push race from the original version is closed by
// store.CreatePendingPayment: it atomically reserves a 'pending' payment
// row for this order (backed by a partial unique index), and we only call
// STKPush if that reservation succeeds. Two concurrent requests for the
// same order can no longer both reach Safaricom.
func InitiatePaymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	userID, ok := middleware.GetUserID(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "please log in again")
		return
	}

	if err := csrf.Verify(r); err != nil {
		writeJSONError(w, http.StatusForbidden, "your session expired, please reload the page")
		return
	}

	orderID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid order id")
		return
	}

	order, err := store.Get().GetOrder(orderID, userID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "order not found")
		return
	}
	if order.Status != "pending" {
		writeJSONError(w, http.StatusConflict, "this order is not payable")
		return
	}

	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	phone := normalizeKenyanPhone(body.Phone)
	if phone == "" {
		writeJSONError(w, http.StatusBadRequest, "enter a valid Safaricom/Airtel number")
		return
	}

	// order.Total can carry cents; M-Pesa only moves whole shillings, so
	// this rounded integer is both what we send to STKPush AND what we
	// record as the payment's amount below — they must always be the same
	// number, or a successful payment will look like an amount mismatch
	// when the callback arrives (see UpdatePaymentResult).
	amount := int(math.Round(order.Total))
	if amount <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid order amount")
		return
	}

	payment, err := store.Get().CreatePendingPayment(orderID, phone, amount)
	if err != nil {
		if err == store.ErrPaymentInProgress {
			writeJSONError(w, http.StatusConflict, "a payment is already in progress for this order — check your phone")
			return
		}
		log.Printf("failed to reserve payment for order %d: %v", order.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to start payment")
		return
	}

	resp, err := mpesaClient.STKPush(phone, amount, fmt.Sprintf("Order%d", order.ID), "Agora order payment")
	if err != nil {
		log.Printf("stk push failed for order %d: %v", order.ID, err)
		if markErr := store.Get().MarkPaymentInitFailed(payment.ID, err.Error()); markErr != nil {
			log.Printf("failed to mark payment %d init_failed: %v", payment.ID, markErr)
		}
		writeJSONError(w, http.StatusBadGateway, "could not reach M-Pesa — please try again in a moment")
		return
	}

	if err := store.Get().AttachSTKDetails(payment.ID, resp.MerchantRequestID, resp.CheckoutRequestID); err != nil {
		log.Printf("failed to attach STK details to payment %d: %v", payment.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to record payment")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":             resp.CustomerMessage,
		"checkout_request_id": resp.CheckoutRequestID,
	})
}

// MpesaCallbackHandler -- PUBLIC endpoint, Safaricom calls this directly.
// See mpesa.go / HARDENING_NOTES.md for the IP-allowlisting requirement —
// application-level amount verification alone is not sufficient.
func MpesaCallbackHandler(w http.ResponseWriter, r *http.Request) {
	var payload mpesa.CallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	cb := payload.Body.StkCallback

	existing, err := store.Get().GetPaymentByCheckoutRequestID(cb.CheckoutRequestID)
	if err != nil {
		log.Printf("mpesa callback for unknown checkout_request_id %q", cb.CheckoutRequestID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if existing.Status != "pending" {
		w.WriteHeader(http.StatusOK)
		return
	}

	status := "failed"
	if cb.ResultCode == 0 {
		amountPaid := payload.Amount()
		// existing.Amount is now guaranteed (by InitiatePaymentHandler) to
		// be the exact rounded shilling amount that was requested, so this
		// comparison is meaningful rather than comparing against an
		// unrounded float total.
		if math.Abs(amountPaid-existing.Amount) > 0.01 {
			log.Printf("mpesa callback amount mismatch for payment %d: expected %.2f got %.2f",
				existing.ID, existing.Amount, amountPaid)
			status = "failed"
		} else {
			status = "success"
		}
	}

	// UpdatePaymentResult restocks the order's items itself if status ends
	// up "failed" — see store.go.
	if err := store.Get().UpdatePaymentResult(cb.CheckoutRequestID, status, payload.MpesaReceipt(), cb.ResultDesc); err != nil {
		log.Printf("failed to update payment result for %q: %v", cb.CheckoutRequestID, err)
	}
	w.WriteHeader(http.StatusOK)
}

// paymentStatusResponse is a deliberately narrow view of models.Payment.
type paymentStatusResponse struct {
	Status       string `json:"status"`
	MpesaReceipt string `json:"mpesa_receipt,omitempty"`
	ResultDesc   string `json:"result_desc,omitempty"`
}

// PaymentStatusHandler -- polled by the front-end after showing "check
// your phone" (GET /orders/{id}/payment-status).
func PaymentStatusHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "please log in again")
		return
	}

	orderID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid order id")
		return
	}

	if _, err := store.Get().GetOrder(orderID, userID); err != nil {
		writeJSONError(w, http.StatusNotFound, "order not found")
		return
	}

	payment, err := store.Get().GetPaymentByOrderID(orderID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no payment found for this order")
		return
	}

	// init_failed is an internal distinction (lets a retry bypass the
	// pending-payment uniqueness guard) — the client only needs to know
	// the push never got anywhere, same as a Safaricom-reported failure.
	status := payment.Status
	if status == "init_failed" {
		status = "failed"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(paymentStatusResponse{
		Status:       status,
		MpesaReceipt: payment.MpesaReceipt,
		ResultDesc:   payment.ResultDesc,
	})
}