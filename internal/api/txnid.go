package api

import (
	"context"

	"github.com/aisa-it/syngo/internal/core"
)

// transactionIDLookup - txn_id, с которыми устройство отправило события (store).
type transactionIDLookup interface {
	TransactionIDsFor(ctx context.Context, userID, deviceID string, eventIDs []string) (map[string]string, error)
}

// attachTransactionIDs кладет unsigned.transaction_id в собственные события
// пользователя, отправленные с устройства сессии: так клиент сопоставляет
// local echo с событием (спека «Local echo», как в Synapse). Другим
// устройствам и пользователям transaction_id не отдается. Один запрос на все
// группы, и только если в них есть свои события.
func (s *Server) attachTransactionIDs(ctx context.Context, session core.Session, groups ...[]eventView) {
	if s.transactionIDs == nil || session.DeviceID == "" {
		return
	}

	ids := make([]string, 0)

	for _, views := range groups {
		for i := range views {
			if views[i].Sender == session.UserID {
				ids = append(ids, views[i].EventID)
			}
		}
	}

	if len(ids) == 0 {
		return
	}

	txns, err := s.transactionIDs.TransactionIDsFor(ctx, session.UserID, session.DeviceID, ids)
	if err != nil {
		s.log.WarnContext(ctx, "api: load transaction ids failed", "error", err)

		return
	}

	if len(txns) == 0 {
		return
	}

	for _, views := range groups {
		for i := range views {
			txnID, ok := txns[views[i].EventID]
			if !ok || views[i].Sender != session.UserID {
				continue
			}

			if views[i].Unsigned == nil {
				views[i].Unsigned = map[string]any{}
			}

			views[i].Unsigned["transaction_id"] = txnID
		}
	}
}
