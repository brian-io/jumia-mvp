// This file is illustrative wiring, not a drop-in package — adapt the
// import paths, router (net/http vs chi/gin/etc.), and session lookup to
// match how the rest of your app is structured. It shows the three
// endpoints the M-Pesa flow needs and how they call into store + mpesa.
package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"

	"agora/mpesa"
	"agora/shared/csrf"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
)



var mpesaClient = mpesa.NewClientFromEnv()

var kenyanPhoneRE = regexp.MustCompile(`^254[17]\d{8}$`)

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// GET /orders/{id}/pay — renders the M-Pesa payment page. Requires login
// and, like every other order-scoped handler here, verifies the order
// belongs to the caller before showing anything about it.
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
			// Nothing to pay — send them to the regular order view instead
			// of showing a payment form for a settled order.
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

// POST /orders/{id}/pay   body: {"phone": "2547XXXXXXXX" or "0712345678" etc.}
// Starts an STK push for an existing order and records a "pending" payment row.
// Always responds JSON — the front-end reads res.ok and the "error" field.
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

	// CSRF is verified via the X-CSRF-Token header here since the body is
	// JSON, not a form — see middleware.VerifyCSRF. The page that renders
	// the payment form (OrderPaymentPageHandler above) must have called
	// middleware.IssueCSRFCookie so this cookie exists.
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
	if !kenyanPhoneRE.MatchString(body.Phone) {
		writeJSONError(w, http.StatusBadRequest, "enter a valid Safaricom/Airtel number")
		return
	}

	amount := int(math.Round(order.Total))

	resp, err := mpesaClient.STKPush(body.Phone, amount, fmt.Sprintf("Order%d", order.ID), "Agora order payment")
	if err != nil {
		log.Printf("stk push failed for order %d: %v", order.ID, err)
		writeJSONError(w, http.StatusBadGateway, "could not reach M-Pesa — please try again in a moment")
		return
	}

	payment, err := store.Get().CreatePayment(models.Payment{
		OrderID:           order.ID,
		Phone:             body.Phone,
		Amount:            order.Total,
		MerchantRequestID: resp.MerchantRequestID,
		CheckoutRequestID: resp.CheckoutRequestID,
		Status:            "pending",
	})
	if err != nil {
		log.Printf("failed to record payment for order %d: %v", order.ID, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to record payment")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":             resp.CustomerMessage,
		"checkout_request_id": payment.CheckoutRequestID,
	})
}

// POST /mpesa/callback -- PUBLIC endpoint, Safaricom calls this directly.
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
		if math.Abs(amountPaid-existing.Amount) > 0.01 {
			log.Printf("mpesa callback amount mismatch for payment %d: expected %.2f got %.2f",
				existing.ID, existing.Amount, amountPaid)
			status = "failed"
		} else {
			status = "success"
		}
	}

	if err := store.Get().UpdatePaymentResult(cb.CheckoutRequestID, status, payload.MpesaReceipt(), cb.ResultDesc); err != nil {
		log.Printf("failed to update payment result for %q: %v", cb.CheckoutRequestID, err)
	}
	w.WriteHeader(http.StatusOK)
}

// paymentStatusResponse is a deliberately narrow view of models.Payment —
// the front-end only needs to know whether to keep waiting, and what to
// show on success/failure. It doesn't need merchant_request_id,
// checkout_request_id, or the phone number echoed back to it.
type paymentStatusResponse struct {
	Status      string `json:"status"`
	MpesaReceipt string `json:"mpesa_receipt,omitempty"`
	ResultDesc  string `json:"result_desc,omitempty"`
}

// GET /orders/{id}/payment-status -- polled by the front-end after
// showing "check your phone". Requires login and ownership of the order,
// same as every other order-scoped handler.
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(paymentStatusResponse{
		Status:       payment.Status,
		MpesaReceipt: payment.MpesaReceipt,
		ResultDesc:   payment.ResultDesc,
	})
}
 
