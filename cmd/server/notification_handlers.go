package main

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/notification"
)

// notificationHandlers implements Phase 7's ops-facing dead-letter-queue
// surface — same shape as settlementHandlers' ops routes, all admin-auth-gated.
type notificationHandlers struct {
	notification *notification.Store
}

type deliveryResponse struct {
	ID            uuid.UUID `json:"id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	EventType     string    `json:"event_type"`
	Channel       string    `json:"channel"`
	Destination   string    `json:"destination"`
	Status        string    `json:"status"`
	AttemptCount  int       `json:"attempt_count"`
	LastError     *string   `json:"last_error,omitempty"`
	NextAttemptAt string    `json:"next_attempt_at"`
	CreatedAt     string    `json:"created_at"`
}

func toDeliveryResponse(d *notification.Delivery) deliveryResponse {
	return deliveryResponse{
		ID:            d.ID,
		TenantID:      d.TenantID,
		EventType:     d.EventType,
		Channel:       string(d.Channel),
		Destination:   d.Destination,
		Status:        string(d.Status),
		AttemptCount:  d.AttemptCount,
		LastError:     d.LastError,
		NextAttemptAt: d.NextAttemptAt.Format(http.TimeFormat),
		CreatedAt:     d.CreatedAt.Format(http.TimeFormat),
	}
}

// GET /admin/notifications/deliveries?status=dead_letter
func (h *notificationHandlers) listDeliveries(w http.ResponseWriter, r *http.Request) {
	status := notification.Status(r.URL.Query().Get("status"))
	if status == "" {
		status = notification.StatusDeadLetter
	}
	deliveries, err := h.notification.ListDeliveries(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]deliveryResponse, len(deliveries))
	for i := range deliveries {
		resp[i] = toDeliveryResponse(&deliveries[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /admin/notifications/deliveries/{deliveryID}/retry
func (h *notificationHandlers) retryDelivery(w http.ResponseWriter, r *http.Request) {
	deliveryID, err := uuid.Parse(chi.URLParam(r, "deliveryID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	d, err := h.notification.RetryDelivery(r.Context(), deliveryID)
	switch {
	case errors.Is(err, notification.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toDeliveryResponse(d))
}
