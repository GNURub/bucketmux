package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestS3NotificationConfigurationDeliversFilteredEvent(t *testing.T) {
	deliveries := make(chan string, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		deliveries <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	if err := handler.svc.UpsertHookFromAdmin(t.Context(), domain.Hook{ID: "s3-events", Name: "S3 events", Kind: domain.HookKindHTTP, URL: receiver.URL, Method: http.MethodPost, Events: []string{domain.HookEventObjectCreated}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	configuration := `<NotificationConfiguration><QueueConfiguration><Id>images-created</Id><Queue>arn:bucketmux:webhook:s3-events</Queue><Event>s3:ObjectCreated:*</Event><Filter><S3Key><FilterRule><Name>prefix</Name><Value>incoming/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`
	request := httptest.NewRequest(http.MethodPut, "/images?notification", strings.NewReader(configuration))
	addAuth(request)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PutBucketNotification status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/images?notification", nil)
	addAuth(request)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "arn:bucketmux:webhook:s3-events") || !strings.Contains(response.Body.String(), "incoming/") {
		t.Fatalf("GetBucketNotification status=%d body=%s", response.Code, response.Body.String())
	}

	put := httptest.NewRequest(http.MethodPut, "/images/incoming/photo.jpg", strings.NewReader("jpeg"))
	addAuth(put)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, put)
	if response.Code != http.StatusOK {
		t.Fatalf("matching PUT status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case payload := <-deliveries:
		for _, expected := range []string{`"eventSource":"aws:s3"`, `"eventName":"ObjectCreated:Put"`, `"configurationId":"images-created"`, `"key":"incoming%2Fphoto.jpg"`} {
			if !strings.Contains(payload, expected) {
				t.Fatalf("notification missing %s: %s", expected, payload)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for S3 event notification")
	}
}
