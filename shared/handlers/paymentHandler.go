// This file is illustrative wiring, not a drop-in package — adapt the
// import paths, router (net/http vs chi/gin/etc.), and session lookup to
// match how the rest of your app is structured. It shows the three
// endpoints the M-Pesa flow needs and how they call into store + mpesa.
package handlers

import (
	"agora/mpesa"
	"agora/shared/models"
	"agora/shared/store"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

var mpesaClient = mpesa.NewClientFromEnv()

// POST /orders/{id}/pay   body: {"phone": "2547XXXXXXXX"}
// Starts an STK push for an existing order and records a "pending" payment row.
func InitiatePaymentHandler(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromSession(r) // however your app resolves the logged-in user
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

	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	resp, err := mpesaClient.STKPush(body.Phone, int(order.Total), fmt.Sprintf("Order%d", order.ID), "Agora order payment")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
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
		http.Error(w, "failed to record payment", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"message":             resp.CustomerMessage,
		"checkout_request_id": payment.CheckoutRequestID,
	})
}

// POST /mpesa/callback -- PUBLIC endpoint, Safaricom calls this directly
// (no session/auth cookie will be present). Always return 200 so
// Safaricom doesn't endlessly retry a payload we've already processed.
func MpesaCallbackHandler(w http.ResponseWriter, r *http.Request) {
	var payload mpesa.CallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	cb := payload.Body.StkCallback

	status := "failed"
	if cb.ResultCode == 0 {
		status = "success"
	}

	store.Get().UpdatePaymentResult(cb.CheckoutRequestID, status, payload.MpesaReceipt(), cb.ResultDesc)
	w.WriteHeader(http.StatusOK)
}

// GET /orders/{id}/payment-status -- lets the frontend poll for the result
// after showing "check your phone".
func PaymentStatusHandler(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}
	payment, err := store.Get().GetPaymentByOrderID(orderID)
	if err != nil {
		http.Error(w, "no payment found for this order", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(payment)
}
