package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

// treasuryWebhookSignatureHeader mirrors settlementWebhookSignatureHeader —
// same placeholder status as treasury.ComputeWebhookSignature's doc comment
// (Busha's real header name isn't known yet).
const treasuryWebhookSignatureHeader = "X-Webhook-Signature"

// treasuryHandlers implements the collection-provider inbound webhook.
// Deliberately unauthenticated by the gateway/adminauth middlewares — it's
// self-verified via treasury.VerifyWebhookSignature instead, same tier as
// /webhooks/settlement/{providerName}.
type treasuryHandlers struct {
	treasury *treasury.Store
}

// POST /webhooks/treasury/busha
func (h *treasuryHandlers) handleDepositWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	signature := r.Header.Get(treasuryWebhookSignatureHeader)

	err = h.treasury.HandleDepositWebhook(r.Context(), body, signature)
	switch {
	case errors.Is(err, treasury.ErrInvalidWebhookSignature):
		writeErr(w, http.StatusUnauthorized, err)
		return
	case errors.Is(err, treasury.ErrUnknownReservation):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
