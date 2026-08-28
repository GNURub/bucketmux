package domain

import "time"

const (
	AuditActionObjectDeleted   = "object.deleted"
	AuditActionProviderDeleted = "provider.deleted"
	AuditActionHookDeleted     = "hook.deleted"
	AuditActionMigrationMove   = "migration.move"
	AuditActionWASMOperation   = "wasm.operation"
)

type AuditEvent struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	TargetID  string    `json:"target_id"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
