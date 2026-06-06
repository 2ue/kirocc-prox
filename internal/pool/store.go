package pool

import "context"

// CredentialStore is the durable source of truth for account credentials.
type CredentialStore interface {
	Load(ctx context.Context) ([]*Credential, error)
	SaveAll(ctx context.Context, creds []*Credential) error
	SaveOne(ctx context.Context, cred *Credential) error
	Delete(ctx context.Context, id string) error
}
