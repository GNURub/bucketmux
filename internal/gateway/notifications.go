package gateway

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
)

type notificationConfiguration struct {
	XMLName        xml.Name                `xml:"NotificationConfiguration"`
	Xmlns          string                  `xml:"xmlns,attr,omitempty"`
	Queues         []notificationTargetXML `xml:"QueueConfiguration"`
	Topics         []notificationTargetXML `xml:"TopicConfiguration"`
	CloudFunctions []notificationTargetXML `xml:"CloudFunctionConfiguration"`
}

type notificationTargetXML struct {
	ID            string              `xml:"Id"`
	Queue         string              `xml:"Queue,omitempty"`
	Topic         string              `xml:"Topic,omitempty"`
	CloudFunction string              `xml:"CloudFunction,omitempty"`
	Events        []string            `xml:"Event"`
	Filter        *notificationFilter `xml:"Filter,omitempty"`
}

type notificationFilter struct {
	Rules []notificationFilterRule `xml:"S3Key>FilterRule"`
}

type notificationFilterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

func (h *Handler) getBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := h.svc.Store.GetBucket(r.Context(), bucket); err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	notifications, err := h.svc.Store.ListBucketNotifications(r.Context(), bucket)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	byID := map[string]*notificationTargetXML{}
	var order []string
	for _, notification := range notifications {
		target := byID[notification.ID]
		if target == nil {
			target = &notificationTargetXML{ID: notification.ID, Queue: "arn:bucketmux:webhook:" + notification.HookID}
			if notification.Prefix != "" || notification.Suffix != "" {
				target.Filter = &notificationFilter{}
				if notification.Prefix != "" {
					target.Filter.Rules = append(target.Filter.Rules, notificationFilterRule{Name: "prefix", Value: notification.Prefix})
				}
				if notification.Suffix != "" {
					target.Filter.Rules = append(target.Filter.Rules, notificationFilterRule{Name: "suffix", Value: notification.Suffix})
				}
			}
			byID[notification.ID] = target
			order = append(order, notification.ID)
		}
		target.Events = append(target.Events, domainEventToS3(notification.Event))
	}
	response := notificationConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, id := range order {
		response.Queues = append(response.Queues, *byID[id])
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}

func (h *Handler) putBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := h.svc.Store.GetBucket(r.Context(), bucket); err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	var request notificationConfiguration
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	targets := append(append(request.Queues, request.Topics...), request.CloudFunctions...)
	var notifications []domain.BucketNotification
	for _, target := range targets {
		if target.ID == "" {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "notification Id is required")
			return
		}
		destination := firstNonEmptyString(target.Queue, target.Topic, target.CloudFunction)
		hookID := strings.TrimPrefix(destination, "arn:bucketmux:webhook:")
		if hookID == destination || hookID == "" {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "destination must use arn:bucketmux:webhook:<hook-id>")
			return
		}
		if _, err := h.svc.Store.GetHook(r.Context(), hookID); err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("hook %q does not exist", hookID))
			return
		}
		prefix, suffix := "", ""
		if target.Filter != nil {
			for _, rule := range target.Filter.Rules {
				switch strings.ToLower(rule.Name) {
				case "prefix":
					prefix = rule.Value
				case "suffix":
					suffix = rule.Value
				default:
					writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "notification filters only support prefix and suffix")
					return
				}
			}
		}
		for _, event := range target.Events {
			domainEvent, ok := s3EventToDomain(event)
			if !ok {
				writeS3Error(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("unsupported event %q", event))
				return
			}
			notifications = append(notifications, domain.BucketNotification{ID: target.ID, Bucket: bucket, HookID: hookID, Event: domainEvent, Prefix: prefix, Suffix: suffix})
		}
	}
	if err := h.svc.Store.ReplaceBucketNotifications(r.Context(), bucket, notifications); err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func s3EventToDomain(event string) (string, bool) {
	switch {
	case strings.HasPrefix(event, "s3:ObjectCreated:"):
		return domain.HookEventObjectCreated, true
	case strings.HasPrefix(event, "s3:ObjectRemoved:"):
		return domain.HookEventObjectDeleted, true
	default:
		return "", false
	}
}

func domainEventToS3(event string) string {
	if event == domain.HookEventObjectDeleted {
		return "s3:ObjectRemoved:*"
	}
	return "s3:ObjectCreated:*"
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
