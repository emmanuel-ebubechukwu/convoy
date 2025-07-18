package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/frain-dev/convoy/database"
	"github.com/frain-dev/convoy/database/hooks"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/pkg/httpheader"
	"github.com/frain-dev/convoy/util"
	"github.com/jmoiron/sqlx"
	"gopkg.in/guregu/null.v4"
)

type eventDeliveryRepo struct {
	db   database.Database
	hook *hooks.Hook
}

var (
	ErrEventDeliveryNotCreated         = errors.New("event delivery could not be created")
	ErrEventDeliveryStatusNotUpdated   = errors.New("event delivery status could not be updated")
	ErrEventDeliveryAttemptsNotUpdated = errors.New("event delivery attempts could not be updated")
	ErrEventDeliveriesNotDeleted       = errors.New("event deliveries could not be deleted")
)

const (
	createEventDelivery = `
    INSERT INTO convoy.event_deliveries (id,project_id,event_id,endpoint_id,device_id,subscription_id,headers,status,metadata,cli_metadata,description,url_query_params,idempotency_key,event_type,acknowledged_at,delivery_mode)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16);
    `
	createEventDeliveries = `
    INSERT INTO convoy.event_deliveries (id,project_id,event_id,endpoint_id,device_id,subscription_id,headers,status,metadata,cli_metadata,description,url_query_params,idempotency_key,event_type,acknowledged_at,delivery_mode)
    VALUES (:id, :project_id, :event_id, :endpoint_id, :device_id, :subscription_id, :headers, :status, :metadata, :cli_metadata, :description, :url_query_params, :idempotency_key, :event_type, :acknowledged_at, :delivery_mode);
    `

	baseFetchEventDelivery = `
    SELECT
        ed.id,ed.project_id,ed.event_id,ed.subscription_id,
        ed.headers,ed.attempts,ed.status,ed.metadata,ed.cli_metadata,
        COALESCE(ed.url_query_params, '') AS url_query_params,
        COALESCE(ed.idempotency_key, '') AS idempotency_key,
        ed.description,ed.created_at,ed.updated_at,ed.acknowledged_at,
        COALESCE(ed.event_type,'') AS "event_type",
        COALESCE(ed.device_id,'') AS "device_id",
        COALESCE(ed.endpoint_id,'') AS "endpoint_id",
        COALESCE(ed.delivery_mode, 'at_least_once')::convoy.delivery_mode AS "delivery_mode",
        COALESCE(ep.id, '') AS "endpoint_metadata.id",
        COALESCE(ep.name, '') AS "endpoint_metadata.name",
        COALESCE(ep.project_id, '') AS "endpoint_metadata.project_id",
        COALESCE(ep.support_email, '') AS "endpoint_metadata.support_email",
        COALESCE(ep.url, '') AS "endpoint_metadata.url",
        COALESCE(ep.owner_id, '') AS "endpoint_metadata.owner_id",

        ev.id AS "event_metadata.id",
        ev.event_type AS "event_metadata.event_type",
		COALESCE(ed.latency_seconds, 0) AS latency_seconds,

		COALESCE(d.id,'') AS "device_metadata.id",
		COALESCE(d.status,'') AS "device_metadata.status",
		COALESCE(d.host_name,'') AS "device_metadata.host_name",

		COALESCE(s.id, '') AS "source_metadata.id",
		COALESCE(s.name, '') AS "source_metadata.name",
		COALESCE(s.idempotency_keys, '{}') AS "source_metadata.idempotency_keys"
    FROM convoy.event_deliveries ed
	LEFT JOIN convoy.endpoints ep ON ed.endpoint_id = ep.id
	LEFT JOIN convoy.events ev ON ed.event_id = ev.id
    LEFT JOIN convoy.devices d ON ed.device_id = d.id
	LEFT JOIN convoy.sources s ON s.id = ev.source_id
	WHERE ed.deleted_at IS NULL
    `

	baseEventDeliveryPagedForward = `
	WITH event_deliveries AS (
	    %s
	    %s
	    AND ed.id <= :cursor
	    ORDER BY ed.id %s
	    LIMIT :limit
	)

	SELECT * FROM event_deliveries ORDER BY id %s
	`

	baseEventDeliveryPagedBackward = `
	WITH event_deliveries AS (
		%s
		%s
		AND ed.id >= :cursor
		ORDER BY ed.id %s
		LIMIT :limit
	)

	SELECT * FROM event_deliveries ORDER BY id %s
	`

	fetchEventDeliveryByID = baseFetchEventDelivery + ` AND ed.id = $1 AND ed.project_id = $2`

	fetchEventDeliverySlim = `
    SELECT
        id,project_id,event_id,subscription_id,
        headers,attempts,status,metadata,cli_metadata,
        COALESCE(url_query_params, '') AS url_query_params,
        COALESCE(idempotency_key, '') AS idempotency_key,created_at,updated_at,
        COALESCE(event_type,'') AS "event_type",
        COALESCE(device_id,'') AS "device_id",
        COALESCE(endpoint_id,'') AS "endpoint_id",
        COALESCE(delivery_mode, 'at_least_once')::convoy.delivery_mode AS "delivery_mode",
        acknowledged_at
    FROM convoy.event_deliveries
	WHERE deleted_at IS NULL
    AND project_id = $1 AND id = $2
    `

	baseEventDeliveryFilter = ` AND (ed.project_id = :project_id OR :project_id = '')
	AND (ed.event_id = :event_id OR :event_id = '')
    AND (ed.event_type = :event_type OR :event_type = '')
	AND ed.created_at >= :start_date
	AND ed.created_at <= :end_date
	AND ed.deleted_at IS NULL`

	countPrevEventDeliveries = `
	select exists(
		SELECT 1
		FROM convoy.event_deliveries ed
		LEFT JOIN convoy.events ev ON ed.event_id = ev.id
		WHERE ed.deleted_at IS NULL
		%s
		AND ed.id > :cursor
		ORDER BY ed.id %s
	);
	`

	loadEventDeliveriesIntervals = `
    SELECT
        DATE_TRUNC('%s', created_at) AS "data.group_only",
        TO_CHAR(DATE_TRUNC('%s', created_at), '%s') AS "data.total_time",
        EXTRACT('%s' FROM created_at) AS "data.index",
        COUNT(*) AS count
        FROM
            convoy.event_deliveries
        WHERE
        project_id = $1 AND
        deleted_at IS NULL AND
        created_at >= $2 AND
        created_at <= $3
        %s
    GROUP BY
        "data.group_only", "data.index";
    `

	fetchEventDeliveries = `
    SELECT
        id,project_id,event_id,subscription_id,
        headers,attempts,status,metadata,cli_metadata,
        COALESCE(ed.idempotency_key, '') AS idempotency_key,
        COALESCE(url_query_params, '') AS url_query_params,
        description,created_at,updated_at,
        COALESCE(event_type,'') AS "event_type",
        COALESCE(device_id,'') AS "device_id",
        COALESCE(endpoint_id,'') AS "endpoint_id",
        COALESCE(delivery_mode, 'at_least_once')::convoy.delivery_mode AS "delivery_mode",
        acknowledged_at
    FROM convoy.event_deliveries ed
    `

	fetchDiscardedEventDeliveries = `
    SELECT
        id,project_id,event_id,subscription_id,
        headers,attempts,status,metadata,cli_metadata,
        COALESCE(idempotency_key, '') AS idempotency_key,
        COALESCE(url_query_params, '') AS url_query_params,
        description,created_at,updated_at,
        COALESCE(event_type,'') AS "event_type",
        COALESCE(device_id,'') AS "device_id",
        COALESCE(delivery_mode, 'at_least_once')::convoy.delivery_mode AS "delivery_mode",
        acknowledged_at
    FROM convoy.event_deliveries
	WHERE status=$1 AND project_id = $2 AND device_id = $3
	AND created_at >= $4 AND created_at <= $5
	AND deleted_at IS NULL;
    `

	fetchStuckEventDeliveries = `
    SELECT id, project_id
    FROM convoy.event_deliveries
	WHERE status = $1
	  AND created_at <= now() - make_interval(secs := 30)
      AND deleted_at IS NULL
    FOR UPDATE SKIP LOCKED
    LIMIT 1000;
    `

	countEventDeliveriesByStatus = `
    SELECT COUNT(id) FROM convoy.event_deliveries WHERE status = $1 AND (project_id = $2 OR $2 = '') AND created_at >= $3 AND created_at <= $4 AND deleted_at IS NULL;
    `

	countEventDeliveries = `
    SELECT COUNT(id) FROM convoy.event_deliveries WHERE (project_id = ? OR ? = '') AND (event_id = ? OR ? = '') AND created_at >= ? AND created_at <= ? AND deleted_at IS NULL
    `

	updateEventDeliveriesStatus = `
    UPDATE convoy.event_deliveries SET status = ?, description = ?, updated_at = NOW() WHERE (project_id = ? OR ? = '')AND id IN (?) AND deleted_at IS NULL;
    `

	updateEventDeliveryMetadata = `
    UPDATE convoy.event_deliveries SET status = $1, metadata = $2, latency_seconds = $3,  updated_at = NOW() WHERE id = $4 AND project_id = $5 AND deleted_at IS NULL;
    `

	softDeleteProjectEventDeliveries = `
    UPDATE convoy.event_deliveries SET deleted_at = NOW() WHERE project_id = $1 AND created_at >= $2 AND created_at <= $3 AND deleted_at IS NULL;
    `

	hardDeleteProjectEventDeliveries = `
    DELETE FROM convoy.event_deliveries WHERE project_id = $1 AND created_at >= $2 AND created_at <= $3;
    `
)

func NewEventDeliveryRepo(db database.Database) datastore.EventDeliveryRepository {
	return &eventDeliveryRepo{db: db, hook: db.GetHook()}
}

func (e *eventDeliveryRepo) CreateEventDelivery(ctx context.Context, delivery *datastore.EventDelivery) error {
	var endpointID *string
	var deviceID *string

	if !util.IsStringEmpty(delivery.EndpointID) {
		endpointID = &delivery.EndpointID
	}

	if !util.IsStringEmpty(delivery.DeviceID) {
		deviceID = &delivery.DeviceID
	}

	if delivery.DeliveryMode == "" {
		delivery.DeliveryMode = datastore.AtLeastOnceDeliveryMode
	}

	tx, isWrapped, err := GetTx(ctx, e.db.GetDB())
	if err != nil {
		return err
	}

	if !isWrapped {
		defer rollbackTx(tx)
	}

	result, err := tx.ExecContext(
		ctx, createEventDelivery, delivery.UID, delivery.ProjectID,
		delivery.EventID, endpointID, deviceID,
		delivery.SubscriptionID, delivery.Headers, delivery.Status,
		delivery.Metadata, delivery.CLIMetadata, delivery.Description, delivery.URLQueryParams, delivery.IdempotencyKey, delivery.EventType,
		delivery.AcknowledgedAt, delivery.DeliveryMode,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected < 1 {
		return ErrEventDeliveryNotCreated
	}

	if isWrapped {
		return nil
	}

	return tx.Commit()
}

// CreateEventDeliveries creates event deliveries in bulk
func (e *eventDeliveryRepo) CreateEventDeliveries(ctx context.Context, deliveries []*datastore.EventDelivery) error {
	values := make([]map[string]interface{}, 0, len(deliveries))

	for _, delivery := range deliveries {
		var endpointID *string
		var deviceID *string

		if !util.IsStringEmpty(delivery.EndpointID) {
			endpointID = &delivery.EndpointID
		}

		if !util.IsStringEmpty(delivery.DeviceID) {
			deviceID = &delivery.DeviceID
		}

		if delivery.DeliveryMode == "" {
			delivery.DeliveryMode = datastore.AtLeastOnceDeliveryMode
		}

		values = append(values, map[string]interface{}{
			"id":               delivery.UID,
			"project_id":       delivery.ProjectID,
			"event_id":         delivery.EventID,
			"endpoint_id":      endpointID,
			"device_id":        deviceID,
			"subscription_id":  delivery.SubscriptionID,
			"headers":          delivery.Headers,
			"status":           delivery.Status,
			"metadata":         delivery.Metadata,
			"cli_metadata":     delivery.CLIMetadata,
			"description":      delivery.Description,
			"url_query_params": delivery.URLQueryParams,
			"idempotency_key":  delivery.IdempotencyKey,
			"event_type":       delivery.EventType,
			"acknowledged_at":  delivery.AcknowledgedAt,
			"delivery_mode":    delivery.DeliveryMode,
		})
	}

	tx, isWrapped, err := GetTx(ctx, e.db.GetDB())
	if err != nil {
		return err
	}

	if !isWrapped {
		defer rollbackTx(tx)
	}

	var j int
	for i := 0; i < len(values); i += PartitionSize {
		j += PartitionSize
		if j > len(values) {
			j = len(values)
		}

		var vs []interface{}
		for _, v := range values[i:j] {
			vs = append(vs, v)
		}

		result, err := tx.NamedExecContext(ctx, createEventDeliveries, vs)
		if err != nil {
			return err
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}

		if len(vs) > 0 && rowsAffected < 1 {
			return ErrEventDeliveryNotCreated
		}
	}

	if isWrapped {
		return nil
	}

	return tx.Commit()
}

func (e *eventDeliveryRepo) FindEventDeliveryByID(ctx context.Context, projectID string, id string) (*datastore.EventDelivery, error) {
	eventDelivery := &datastore.EventDelivery{}
	err := e.db.GetDB().QueryRowxContext(ctx, fetchEventDeliveryByID, id, projectID).StructScan(eventDelivery)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, datastore.ErrEventDeliveryNotFound
		}
		return nil, err
	}

	return eventDelivery, nil
}

func (e *eventDeliveryRepo) FindEventDeliveryByIDSlim(ctx context.Context, projectID string, id string) (*datastore.EventDelivery, error) {
	eventDelivery := &datastore.EventDelivery{}
	err := e.db.GetDB().QueryRowxContext(ctx, fetchEventDeliverySlim, projectID, id).StructScan(eventDelivery)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, datastore.ErrEventDeliveryNotFound
		}
		return nil, err
	}

	return eventDelivery, nil
}

func (e *eventDeliveryRepo) FindEventDeliveriesByIDs(ctx context.Context, projectID string, ids []string) ([]datastore.EventDelivery, error) {
	eventDeliveries := make([]datastore.EventDelivery, 0)
	query := fetchEventDeliveries + " WHERE id IN (?) AND project_id = ? AND deleted_at IS NULL"

	query, args, err := sqlx.In(query, ids, projectID)
	if err != nil {
		return nil, err
	}

	query = e.db.GetDB().Rebind(query)

	rows, err := e.db.GetReadDB().QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer closeWithError(rows)

	for rows.Next() {
		var ed datastore.EventDelivery
		err = rows.StructScan(&ed)
		if err != nil {
			return nil, err
		}

		eventDeliveries = append(eventDeliveries, ed)
	}

	return eventDeliveries, nil
}

func (e *eventDeliveryRepo) FindEventDeliveriesByEventID(ctx context.Context, projectID string, eventID string) ([]datastore.EventDelivery, error) {
	eventDeliveries := make([]datastore.EventDelivery, 0)

	q := fetchEventDeliveries + " WHERE event_id = $1 AND project_id = $2 AND deleted_at IS NULL"
	rows, err := e.db.GetReadDB().QueryxContext(ctx, q, eventID, projectID)
	if err != nil {
		return nil, err
	}
	defer closeWithError(rows)

	for rows.Next() {
		var ed datastore.EventDelivery
		err = rows.StructScan(&ed)
		if err != nil {
			return nil, err
		}

		eventDeliveries = append(eventDeliveries, ed)
	}

	return eventDeliveries, nil
}

func (e *eventDeliveryRepo) CountDeliveriesByStatus(ctx context.Context, projectID string, status datastore.EventDeliveryStatus, params datastore.SearchParams) (int64, error) {
	deliveriesCount := struct{ Count int64 }{}

	start := time.Unix(params.CreatedAtStart, 0)
	end := time.Unix(params.CreatedAtEnd, 0)
	err := e.db.GetReadDB().QueryRowxContext(ctx, countEventDeliveriesByStatus, status, projectID, start, end).StructScan(&deliveriesCount)
	if err != nil {
		return 0, err
	}

	return deliveriesCount.Count, nil
}

func (e *eventDeliveryRepo) FindStuckEventDeliveriesByStatus(ctx context.Context, status datastore.EventDeliveryStatus) ([]datastore.EventDelivery, error) {
	eventDeliveries := make([]datastore.EventDelivery, 0)

	rows, err := e.db.GetReadDB().QueryxContext(ctx, fetchStuckEventDeliveries, status)
	if err != nil {
		return nil, err
	}
	defer closeWithError(rows)

	for rows.Next() {
		var ed datastore.EventDelivery
		err = rows.StructScan(&ed)
		if err != nil {
			return nil, err
		}

		eventDeliveries = append(eventDeliveries, ed)
	}

	return eventDeliveries, nil
}

func (e *eventDeliveryRepo) UpdateStatusOfEventDelivery(ctx context.Context, projectID string, delivery datastore.EventDelivery, status datastore.EventDeliveryStatus) error {
	query, args, err := sqlx.In(updateEventDeliveriesStatus, status, delivery.Description, projectID, projectID, []string{delivery.UID})
	if err != nil {
		return err
	}

	query = e.db.GetDB().Rebind(query)

	result, err := e.db.GetDB().ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected < 1 {
		return ErrEventDeliveryStatusNotUpdated
	}

	return nil
}

func (e *eventDeliveryRepo) UpdateStatusOfEventDeliveries(ctx context.Context, projectID string, ids []string, status datastore.EventDeliveryStatus) error {
	query, args, err := sqlx.In(updateEventDeliveriesStatus, status, "", projectID, projectID, ids)
	if err != nil {
		return err
	}

	query = e.db.GetDB().Rebind(query)

	result, err := e.db.GetDB().ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected < 1 {
		return ErrEventDeliveryStatusNotUpdated
	}

	return nil
}

func (e *eventDeliveryRepo) FindDiscardedEventDeliveries(ctx context.Context, projectID, deviceId string, searchParams datastore.SearchParams) ([]datastore.EventDelivery, error) {
	eventDeliveries := make([]datastore.EventDelivery, 0)

	start := time.Unix(searchParams.CreatedAtStart, 0)
	end := time.Unix(searchParams.CreatedAtEnd, 0)

	rows, err := e.db.GetReadDB().QueryxContext(ctx, fetchDiscardedEventDeliveries, datastore.DiscardedEventStatus, projectID, deviceId, start, end)
	if err != nil {
		return nil, err
	}
	defer closeWithError(rows)

	for rows.Next() {
		var ed datastore.EventDelivery
		err = rows.StructScan(&ed)
		if err != nil {
			return nil, err
		}

		eventDeliveries = append(eventDeliveries, ed)
	}

	return eventDeliveries, nil
}

func (e *eventDeliveryRepo) UpdateEventDeliveryMetadata(ctx context.Context, projectID string, delivery *datastore.EventDelivery) error {
	result, err := e.db.GetDB().ExecContext(ctx, updateEventDeliveryMetadata, delivery.Status, delivery.Metadata, delivery.LatencySeconds, delivery.UID, projectID)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected < 1 {
		return ErrEventDeliveryAttemptsNotUpdated
	}

	e.hook.Fire(ctx, datastore.EventDeliveryUpdated, delivery, nil)

	return nil
}

func (e *eventDeliveryRepo) CountEventDeliveries(ctx context.Context, projectID string, endpointIDs []string, eventID string, status []datastore.EventDeliveryStatus, params datastore.SearchParams) (int64, error) {
	count := struct {
		Count int64
	}{}

	start := time.Unix(params.CreatedAtStart, 0)
	end := time.Unix(params.CreatedAtEnd, 0)

	args := []interface{}{
		projectID, projectID,
		eventID, eventID,
		start, end,
	}

	q := countEventDeliveries

	if len(endpointIDs) > 0 {
		q += ` AND endpoint_id IN (?)`
		args = append(args, endpointIDs)
	}

	if len(status) > 0 {
		q += ` AND status IN (?)`
		args = append(args, status)
	}

	query, args, err := sqlx.In(q, args...)
	if err != nil {
		return 0, err
	}

	query = e.db.GetReadDB().Rebind(query)

	err = e.db.GetReadDB().QueryRowxContext(ctx, query, args...).StructScan(&count)
	if err != nil {
		return 0, err
	}

	return count.Count, nil
}

func (e *eventDeliveryRepo) DeleteProjectEventDeliveries(ctx context.Context, projectID string, filter *datastore.EventDeliveryFilter, hardDelete bool) error {
	var result sql.Result
	var err error

	start := time.Unix(filter.CreatedAtStart, 0)
	end := time.Unix(filter.CreatedAtEnd, 0)

	if hardDelete {
		result, err = e.db.GetDB().ExecContext(ctx, hardDeleteProjectEventDeliveries, projectID, start, end)
	} else {
		result, err = e.db.GetDB().ExecContext(ctx, softDeleteProjectEventDeliveries, projectID, start, end)
	}

	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected < 1 {
		return ErrEventDeliveriesNotDeleted
	}

	return nil
}

func (e *eventDeliveryRepo) LoadEventDeliveriesPaged(ctx context.Context, projectID string, endpointIDs []string, eventID, subscriptionID string, status []datastore.EventDeliveryStatus, params datastore.SearchParams, pageable datastore.Pageable, idempotencyKey, eventType string) ([]datastore.EventDelivery, datastore.PaginationData, error) {
	eventDeliveriesP := make([]EventDeliveryPaginated, 0)

	start := time.Unix(params.CreatedAtStart, 0)
	end := time.Unix(params.CreatedAtEnd, 0)

	arg := map[string]interface{}{
		"endpoint_ids":    endpointIDs,
		"project_id":      projectID,
		"limit":           pageable.Limit(),
		"subscription_id": subscriptionID,
		"start_date":      start,
		"event_id":        eventID,
		"event_type":      eventType,
		"end_date":        end,
		"status":          status,
		"cursor":          pageable.Cursor(),
		"idempotency_key": idempotencyKey,
	}

	var query, filterQuery string
	if pageable.Direction == datastore.Next {
		query = getFwdDeliveryPageQuery(pageable.SortOrder())
	} else {
		query = getBackwardDeliveryPageQuery(pageable.SortOrder())
	}

	filterQuery = baseEventDeliveryFilter
	if len(endpointIDs) > 0 {
		filterQuery += ` AND ed.endpoint_id IN (:endpoint_ids)`
	}

	if len(status) > 0 {
		filterQuery += ` AND ed.status IN (:status)`
	}

	if !util.IsStringEmpty(subscriptionID) {
		filterQuery += ` AND ed.subscription_id = :subscription_id`
	}

	preOrder := pageable.SortOrder()
	if pageable.Direction == datastore.Prev {
		preOrder = reverseOrder(preOrder)
	}

	query = fmt.Sprintf(query, baseFetchEventDelivery, filterQuery, preOrder, pageable.SortOrder())

	query, args, err := sqlx.Named(query, arg)
	if err != nil {
		return nil, datastore.PaginationData{}, err
	}

	query, args, err = sqlx.In(query, args...)
	if err != nil {
		return nil, datastore.PaginationData{}, err
	}

	query = e.db.GetReadDB().Rebind(query)
	// Replace single colons with double colons to handle PostgreSQL type casting
	// This is needed because sqlx.Named() generates :param but PostgreSQL expects ::param for type casting
	query = strings.ReplaceAll(query, ":", "::")

	rows, err := e.db.GetReadDB().QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, datastore.PaginationData{}, err
	}

	for rows.Next() {
		var ed EventDeliveryPaginated
		err = rows.StructScan(&ed)
		if err != nil {
			return nil, datastore.PaginationData{}, err
		}

		eventDeliveriesP = append(eventDeliveriesP, ed)
	}

	eventDeliveries := make([]datastore.EventDelivery, 0, len(eventDeliveriesP))

	for i := range eventDeliveriesP {
		ev := &eventDeliveriesP[i]
		var cli *datastore.CLIMetadata
		if ev.CLIMetadata != nil {
			cli = &datastore.CLIMetadata{
				EventType: ev.CLIMetadata.EventType.ValueOrZero(),
				SourceID:  ev.CLIMetadata.SourceID.ValueOrZero(),
			}
		}

		eventDeliveries = append(eventDeliveries, datastore.EventDelivery{
			UID:            ev.UID,
			ProjectID:      ev.ProjectID,
			EventID:        ev.EventID,
			EndpointID:     ev.EndpointID,
			DeviceID:       ev.DeviceID,
			SubscriptionID: ev.SubscriptionID,
			IdempotencyKey: ev.IdempotencyKey,
			Headers:        ev.Headers,
			URLQueryParams: ev.URLQueryParams,
			Latency:        ev.Latency,
			LatencySeconds: ev.LatencySeconds,
			EventType:      ev.EventType,
			Endpoint: &datastore.Endpoint{
				UID:          ev.Endpoint.UID.ValueOrZero(),
				ProjectID:    ev.Endpoint.ProjectID.ValueOrZero(),
				Url:          ev.Endpoint.URL.ValueOrZero(),
				Name:         ev.Endpoint.Name.ValueOrZero(),
				SupportEmail: ev.Endpoint.SupportEmail.ValueOrZero(),
				OwnerID:      ev.Endpoint.OwnerID.ValueOrZero(),
			},
			Source: &datastore.Source{
				UID:             ev.Source.UID.ValueOrZero(),
				Name:            ev.Source.Name.ValueOrZero(),
				IdempotencyKeys: ev.Source.IdempotencyKeys,
			},
			Device: &datastore.Device{
				UID:      ev.Device.UID.ValueOrZero(),
				HostName: ev.Device.HostName.ValueOrZero(),
				Status:   datastore.DeviceStatus(ev.Device.Status.ValueOrZero()),
			},
			Event:          &datastore.Event{EventType: datastore.EventType(ev.Event.EventType.ValueOrZero())},
			Status:         ev.Status,
			Metadata:       ev.Metadata,
			CLIMetadata:    cli,
			Description:    ev.Description,
			AcknowledgedAt: ev.AcknowledgedAt,
			DeliveryMode:   ev.DeliveryMode,
			CreatedAt:      ev.CreatedAt,
			UpdatedAt:      ev.UpdatedAt,
			DeletedAt:      ev.DeletedAt,
		})
	}

	var rowCount datastore.PrevRowCount
	if len(eventDeliveries) > 0 {
		var countQuery string
		var qargs []interface{}
		first := eventDeliveries[0]
		qarg := arg
		qarg["cursor"] = first.UID

		tmp := getCountEventPrevRowQuery(pageable.SortOrder())

		cq := fmt.Sprintf(tmp, filterQuery, pageable.SortOrder())
		countQuery, qargs, err = sqlx.Named(cq, qarg)
		if err != nil {
			return nil, datastore.PaginationData{}, err
		}

		countQuery, qargs, err = sqlx.In(countQuery, qargs...)
		if err != nil {
			return nil, datastore.PaginationData{}, err
		}

		countQuery = e.db.GetReadDB().Rebind(countQuery)

		// count the row number before the first row
		rows, err = e.db.GetReadDB().QueryxContext(ctx, countQuery, qargs...)
		if err != nil {
			return nil, datastore.PaginationData{}, err
		}
		defer closeWithError(rows)

		if rows.Next() {
			err = rows.StructScan(&rowCount)
			if err != nil {
				return nil, datastore.PaginationData{}, err
			}
		}
	}

	ids := make([]string, len(eventDeliveries))
	for i := range eventDeliveries {
		ids[i] = eventDeliveries[i].UID
	}

	if len(eventDeliveries) > pageable.PerPage {
		eventDeliveries = eventDeliveries[:len(eventDeliveries)-1]
	}

	pagination := &datastore.PaginationData{PrevRowCount: rowCount}
	pagination = pagination.Build(pageable, ids)

	return eventDeliveries, *pagination, nil
}

const (
	dailyIntervalFormat   = "yyyy-mm-dd"        // 1 day
	weeklyIntervalFormat  = dailyIntervalFormat // 1 week
	monthlyIntervalFormat = "yyyy-mm"           // 1 month
	yearlyIntervalFormat  = "yyyy"              // 1 month
)

func (e *eventDeliveryRepo) LoadEventDeliveriesIntervals(ctx context.Context, projectID string, params datastore.SearchParams, period datastore.Period, endpointIds []string) ([]datastore.EventInterval, error) {
	intervals := make([]datastore.EventInterval, 0)

	start := time.Unix(params.CreatedAtStart, 0)
	end := time.Unix(params.CreatedAtEnd, 0)

	var timeComponent string
	var format string
	var extract string
	switch period {
	case datastore.Daily:
		timeComponent = "day"
		format = dailyIntervalFormat
		extract = "doy"
	case datastore.Weekly:
		timeComponent = "week"
		format = weeklyIntervalFormat
		extract = timeComponent
	case datastore.Monthly:
		timeComponent = "month"
		format = monthlyIntervalFormat
		extract = timeComponent
	case datastore.Yearly:
		timeComponent = "year"
		format = yearlyIntervalFormat
		extract = timeComponent
	default:
		return nil, errors.New("specified data cannot be generated for period")
	}

	filter := ""
	var args = []interface{}{projectID, start, end}
	if len(endpointIds) > 0 {
		filter = "AND endpoint_id = ANY($4)"
		args = append(args, pq.Array(endpointIds))
	}
	q := fmt.Sprintf(loadEventDeliveriesIntervals, timeComponent, timeComponent, format, extract, filter)
	rows, err := e.db.GetReadDB().QueryxContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		var interval datastore.EventInterval
		err = rows.StructScan(&interval)
		if err != nil {
			return nil, err
		}

		intervals = append(intervals, interval)
	}

	if len(intervals) < minLen {
		var d time.Duration
		switch period {
		case datastore.Daily:
			d = time.Hour * 24
		case datastore.Weekly:
			d = time.Hour * 24 * 7
		case datastore.Monthly:
			d = time.Hour * 24 * 30
		case datastore.Yearly:
			d = time.Hour * 24 * 365
		}
		intervals, err = padIntervals(intervals, d, period)
		if err != nil {
			return nil, err
		}
	}

	return intervals, nil
}

func (e *eventDeliveryRepo) ExportRecords(ctx context.Context, projectID string, createdAt time.Time, w io.Writer) (int64, error) {
	return exportRecords(ctx, e.db.GetReadDB(), "convoy.event_deliveries", projectID, createdAt, w)
}

const minLen = 30

func padIntervals(intervals []datastore.EventInterval, duration time.Duration, period datastore.Period) ([]datastore.EventInterval, error) {
	var err error

	var format string

	switch period {
	case datastore.Daily:
		format = "2006-01-02"
	case datastore.Weekly:
		format = "2006-01-02"
	case datastore.Monthly:
		format = "2006-01"
	case datastore.Yearly:
		format = "2006"
	default:
		return nil, errors.New("specified data cannot be generated for period")
	}

	start := time.Now()
	if len(intervals) > 0 {
		start, err = time.Parse(format, intervals[0].Data.Time)
		if err != nil {
			return nil, err
		}
		start = start.Add(-duration) // take it back once here, since we getting it from the original slice
	}

	numPadding := minLen - (len(intervals))
	paddedIntervals := make([]datastore.EventInterval, numPadding, numPadding+len(intervals))
	for i := numPadding; i > 0; i-- {
		paddedIntervals[i-1] = datastore.EventInterval{
			Data: datastore.EventIntervalData{
				Interval: 0,
				Time:     start.Format(format),
			},
			Count: 0,
		}
		start = start.Add(-duration)
	}

	paddedIntervals = append(paddedIntervals, intervals...)

	return paddedIntervals, nil
}

type EndpointMetadata struct {
	UID          null.String `db:"id"`
	Name         null.String `db:"name"`
	URL          null.String `db:"url"`
	ProjectID    null.String `db:"project_id"`
	SupportEmail null.String `db:"support_email"`
	OwnerID      null.String `db:"owner_id"`
}

type EventMetadata struct {
	UID       null.String `db:"id"`
	EventType null.String `db:"event_type"`
}

type SourceMetadata struct {
	UID             null.String    `db:"id"`
	Name            null.String    `db:"name"`
	IdempotencyKeys pq.StringArray `db:"idempotency_keys"`
}

type DeviceMetadata struct {
	UID      null.String `db:"id"`
	Status   null.String `json:"status" db:"status"`
	HostName null.String `json:"host_name" db:"host_name"`
}

type CLIMetadata struct {
	EventType null.String `json:"event_type" db:"event_type"`
	SourceID  null.String `json:"source_id" db:"source_id"`
}

type EventDeliveryPaginated struct {
	UID            string                `json:"uid" db:"id"`
	ProjectID      string                `json:"project_id,omitempty" db:"project_id"`
	EventID        string                `json:"event_id,omitempty" db:"event_id"`
	EndpointID     string                `json:"endpoint_id,omitempty" db:"endpoint_id"`
	DeviceID       string                `json:"device_id" db:"device_id"`
	SubscriptionID string                `json:"subscription_id,omitempty" db:"subscription_id"`
	Headers        httpheader.HTTPHeader `json:"headers" db:"headers"`
	URLQueryParams string                `json:"url_query_params" db:"url_query_params"`
	IdempotencyKey string                `json:"idempotency_key" db:"idempotency_key"`
	// Deprecated: Latency is deprecated.
	Latency        string              `json:"latency" db:"latency"`
	LatencySeconds float64             `json:"latency_seconds" db:"latency_seconds"`
	EventType      datastore.EventType `json:"event_type,omitempty" db:"event_type"`

	Endpoint *EndpointMetadata `json:"endpoint_metadata,omitempty" db:"endpoint_metadata"`
	Event    *EventMetadata    `json:"event_metadata,omitempty" db:"event_metadata"`
	Source   *SourceMetadata   `json:"source_metadata,omitempty" db:"source_metadata"`
	Device   *DeviceMetadata   `json:"device_metadata,omitempty" db:"device_metadata"`

	DeliveryAttempts datastore.DeliveryAttempts    `json:"-" db:"attempts"`
	Status           datastore.EventDeliveryStatus `json:"status" db:"status"`
	Metadata         *datastore.Metadata           `json:"metadata" db:"metadata"`
	CLIMetadata      *CLIMetadata                  `json:"cli_metadata" db:"cli_metadata"`
	Description      string                        `json:"description,omitempty" db:"description"`
	AcknowledgedAt   null.Time                     `json:"acknowledged_at,omitempty" db:"acknowledged_at,omitempty" swaggertype:"string"`
	CreatedAt        time.Time                     `json:"created_at,omitempty" db:"created_at,omitempty" swaggertype:"string"`
	UpdatedAt        time.Time                     `json:"updated_at,omitempty" db:"updated_at,omitempty" swaggertype:"string"`
	DeletedAt        null.Time                     `json:"deleted_at,omitempty" db:"deleted_at" swaggertype:"string"`
	DeliveryMode     datastore.DeliveryMode        `json:"delivery_mode" db:"delivery_mode"`
}

func (m *CLIMetadata) Scan(value interface{}) error {
	if value == nil {
		return nil
	}

	b, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("unsupported value type %T", value)
	}

	if string(b) == "null" {
		return nil
	}

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	return nil
}

func getFwdDeliveryPageQuery(sortOrder string) string {
	if sortOrder == "ASC" {
		return strings.Replace(baseEventDeliveryPagedForward, "<=", ">=", 1)
	}

	return baseEventDeliveryPagedForward
}

func getBackwardDeliveryPageQuery(sortOrder string) string {
	if sortOrder == "ASC" {
		return strings.Replace(baseEventDeliveryPagedBackward, ">=", "<=", 1)
	}

	return baseEventDeliveryPagedBackward
}

func getCountEventPrevRowQuery(sortOrder string) string {
	if sortOrder == "ASC" {
		return strings.Replace(countPrevEventDeliveries, ">", "<", 1)
	}

	return countPrevEventDeliveries
}

func reverseOrder(sortOrder string) string {
	switch sortOrder {
	case "ASC":
		return "DESC"
	default:
		return "ASC"
	}
}

func (e *eventDeliveryRepo) PartitionEventDeliveriesTable(ctx context.Context) error {
	_, err := e.db.GetDB().ExecContext(ctx, partitionEventDeliveriesTable)
	if err != nil {
		return err
	}

	return nil
}

func (e *eventDeliveryRepo) UnPartitionEventDeliveriesTable(ctx context.Context) error {
	_, err := e.db.GetDB().ExecContext(ctx, unPartitionEventDeliveriesTable)
	if err != nil {
		return err
	}

	return nil
}

var partitionEventDeliveriesTable = `
CREATE OR REPLACE FUNCTION enforce_event_delivery_fk()
    RETURNS TRIGGER AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM convoy.event_deliveries
        WHERE id = NEW.event_delivery_id
    ) THEN
        RAISE EXCEPTION 'Foreign key violation: event_delivery_id % does not exist in event deliveries', NEW.event_delivery_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION partition_event_deliveries_table()
    RETURNS VOID AS $$
DECLARE
    r RECORD;
BEGIN
    RAISE NOTICE 'Creating partitioned event deliveries table...';

    -- Drop old partitioned table
    DROP TABLE IF EXISTS convoy.event_deliveries_new;

    -- Create partitioned table
   create table convoy.event_deliveries_new
    (
        id               VARCHAR not null,
        status           TEXT    not null,
        description      TEXT    not null,
        project_id       VARCHAR not null references convoy.projects,
        endpoint_id      VARCHAR references convoy.endpoints,
        event_id         VARCHAR not null,
        device_id        VARCHAR references convoy.devices,
        subscription_id  VARCHAR not null references convoy.subscriptions,
        metadata         jsonb   not null,
        headers          jsonb,
        attempts         bytea,
        cli_metadata     jsonb,
        created_at       TIMESTAMP WITH TIME ZONE default CURRENT_TIMESTAMP,
        updated_at       TIMESTAMP WITH TIME ZONE default CURRENT_TIMESTAMP,
        deleted_at       TIMESTAMP WITH TIME ZONE,
        url_query_params VARCHAR,
        idempotency_key  TEXT,
        latency          TEXT,
        event_type       TEXT,
        acknowledged_at  TIMESTAMP WITH TIME ZONE,
        latency_seconds  NUMERIC,
        delivery_mode    convoy.delivery_mode NOT NULL DEFAULT 'at_least_once',
        PRIMARY KEY (id, created_at, project_id)
    ) PARTITION BY RANGE (project_id, created_at);

    RAISE NOTICE 'Creating partitions...';
    FOR r IN
        WITH dates AS (
            SELECT project_id, created_at::DATE
            FROM convoy.event_deliveries
            GROUP BY created_at::DATE, project_id
            order by created_at::DATE
        )
        SELECT project_id,
               created_at::TEXT AS start_date,
               (created_at + 1)::TEXT AS stop_date,
               'event_deliveries_' || pg_catalog.REPLACE(project_id::TEXT, '-', '') || '_' || pg_catalog.REPLACE(created_at::TEXT, '-', '') AS partition_table_name
        FROM dates
    LOOP
        EXECUTE FORMAT(
            'CREATE TABLE IF NOT EXISTS convoy.%s PARTITION OF convoy.event_deliveries_new FOR VALUES FROM (%L, %L) TO (%L, %L)',
            r.partition_table_name, r.project_id, r.start_date, r.project_id, r.stop_date
        );
    END LOOP;

    RAISE NOTICE 'Migrating data...';
    INSERT INTO convoy.event_deliveries_new (
        id, status, description, project_id, created_at, updated_at, endpoint_id, event_id, device_id, subscription_id, metadata, headers,
        attempts, cli_metadata, deleted_at, url_query_params, idempotency_key, latency, event_type, acknowledged_at,
        latency_seconds, delivery_mode
    )
    SELECT id, status, description, project_id, created_at, updated_at, endpoint_id, event_id, device_id, subscription_id, metadata, headers,
           attempts, cli_metadata, deleted_at, url_query_params, idempotency_key, latency, event_type, acknowledged_at,
           latency_seconds, COALESCE(delivery_mode, 'at_least_once')::convoy.delivery_mode
    FROM convoy.event_deliveries;

    -- Manage table renaming
    ALTER TABLE convoy.delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_event_delivery_id_fkey;
    ALTER TABLE convoy.event_deliveries RENAME TO event_deliveries_old;
    ALTER TABLE convoy.event_deliveries_new RENAME TO event_deliveries;
    DROP TABLE IF EXISTS convoy.event_deliveries_old;

    RAISE NOTICE 'Recreating indexes...';
    create index event_deliveries_event_type on convoy.event_deliveries (event_type);
    create index idx_event_deliveries_created_at_key on convoy.event_deliveries (created_at);
    create index idx_event_deliveries_deleted_at_key on convoy.event_deliveries (deleted_at);
    create index idx_event_deliveries_device_id_key on convoy.event_deliveries (device_id);
    create index idx_event_deliveries_endpoint_id_key on convoy.event_deliveries (endpoint_id);
    create index idx_event_deliveries_event_id_key on convoy.event_deliveries (event_id);
    create index idx_event_deliveries_project_id_endpoint_id on convoy.event_deliveries (project_id, endpoint_id);
    create index idx_event_deliveries_project_id_endpoint_id_status on convoy.event_deliveries (project_id, endpoint_id, status);
    create index idx_event_deliveries_project_id_event_id on convoy.event_deliveries (project_id, event_id);
    create index idx_event_deliveries_project_id_key on convoy.event_deliveries (project_id);
    create index idx_event_deliveries_status on convoy.event_deliveries (status);
    create index idx_event_deliveries_status_key on convoy.event_deliveries (status);

    -- Recreate FK using trigger
    CREATE OR REPLACE TRIGGER event_delivery_fk_check
    BEFORE INSERT ON convoy.delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION enforce_event_delivery_fk();

    RAISE NOTICE 'Migration complete!';
END;
$$ LANGUAGE plpgsql;
select partition_event_deliveries_table();
`

var unPartitionEventDeliveriesTable = `
create or replace function convoy.un_partition_event_deliveries_table() returns VOID as $$
begin
	RAISE NOTICE 'Starting un-partitioning of event deliveries table...';

	-- Drop old partitioned table
    DROP TABLE IF EXISTS convoy.event_deliveries_new;

    -- Create partitioned table
    CREATE TABLE convoy.event_deliveries_new
    (
        id               VARCHAR not null primary key ,
        status           TEXT    not null,
        description      TEXT    not null,
        project_id       VARCHAR not null references convoy.projects,
        endpoint_id      VARCHAR references convoy.endpoints,
        event_id         VARCHAR not null,
        device_id        VARCHAR references convoy.devices,
        subscription_id  VARCHAR not null references convoy.subscriptions,
        metadata         jsonb   not null,
        headers          jsonb,
        attempts         bytea,
        cli_metadata     jsonb,
        created_at       TIMESTAMP WITH TIME ZONE default CURRENT_TIMESTAMP,
        updated_at       TIMESTAMP WITH TIME ZONE default CURRENT_TIMESTAMP,
        deleted_at       TIMESTAMP WITH TIME ZONE,
        url_query_params VARCHAR,
        idempotency_key  TEXT,
        latency          TEXT,
        event_type       TEXT,
        acknowledged_at  TIMESTAMP WITH TIME ZONE,
        latency_seconds  NUMERIC,
        delivery_mode    convoy.delivery_mode NOT NULL DEFAULT 'at_least_once'
    );

    RAISE NOTICE 'Migrating data...';
    INSERT INTO convoy.event_deliveries_new (
        id, status, description, project_id, created_at, updated_at, endpoint_id, event_id, device_id, subscription_id, metadata, headers,
        attempts, cli_metadata, deleted_at, url_query_params, idempotency_key, latency, event_type, acknowledged_at,
        latency_seconds, delivery_mode
    )
    SELECT id, status, description, project_id, created_at, updated_at, endpoint_id, event_id, device_id, subscription_id, metadata, headers,
           attempts, cli_metadata, deleted_at, url_query_params, idempotency_key, latency, event_type, acknowledged_at,
           latency_seconds, COALESCE(delivery_mode, 'at_least_once')::convoy.delivery_mode
    FROM convoy.event_deliveries;

    ALTER TABLE convoy.delivery_attempts DROP CONSTRAINT if exists delivery_attempts_event_delivery_id_fkey;
    ALTER TABLE convoy.delivery_attempts
        ADD CONSTRAINT delivery_attempts_event_delivery_id_fkey
            FOREIGN KEY (event_delivery_id) REFERENCES convoy.event_deliveries_new (id);

    ALTER TABLE convoy.event_deliveries RENAME TO event_deliveries_old;
    ALTER TABLE convoy.event_deliveries_new RENAME TO event_deliveries;
    DROP TABLE IF EXISTS convoy.event_deliveries_old;

    RAISE NOTICE 'Recreating indexes...';
    create index event_deliveries_event_type on convoy.event_deliveries (event_type);
    create index idx_event_deliveries_created_at_key on convoy.event_deliveries (created_at);
    create index idx_event_deliveries_deleted_at_key on convoy.event_deliveries (deleted_at);
    create index idx_event_deliveries_device_id_key on convoy.event_deliveries (device_id);
    create index idx_event_deliveries_endpoint_id_key on convoy.event_deliveries (endpoint_id);
    create index idx_event_deliveries_event_id_key on convoy.event_deliveries (event_id);
    create index idx_event_deliveries_project_id_endpoint_id on convoy.event_deliveries (project_id, endpoint_id);
    create index idx_event_deliveries_project_id_endpoint_id_status on convoy.event_deliveries (project_id, endpoint_id, status);
    create index idx_event_deliveries_project_id_event_id on convoy.event_deliveries (project_id, event_id);
    create index idx_event_deliveries_project_id_key on convoy.event_deliveries (project_id);
    create index idx_event_deliveries_status on convoy.event_deliveries (status);
    create index idx_event_deliveries_status_key on convoy.event_deliveries (status);

	RAISE NOTICE 'Successfully un-partitioned events table...';
end $$ language plpgsql;
select convoy.un_partition_event_deliveries_table()
`
