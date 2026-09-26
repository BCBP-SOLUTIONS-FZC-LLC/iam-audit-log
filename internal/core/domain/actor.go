package domain

import "regexp"

// ActorType is who performed the action (LLD §4.1, §10.3).
type ActorType string

// ActorType values — the audit_actor_type enum.
const (
	ActorUser           ActorType = "user"
	ActorServiceAccount ActorType = "service_account"
	ActorIAMSystem      ActorType = "iam_system"
	// ActorAnonymous is an Audit-Log-only value for pre-auth events with no
	// resolvable principal: actor_id SHALL be NULL and any claimed identity
	// lives only in metadata (AL-D11). It is not an IAM principal.
	ActorAnonymous ActorType = "anonymous"
)

// ActorRef attributes an audit row (actor_display is a label only and is
// never consulted for authorization).
type ActorRef struct {
	Type    ActorType
	ID      string // canonical UUID; "" iff Type == ActorAnonymous
	Display string
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
func IsUUID(s string) bool { return uuidShape.MatchString(s) }

// Validate enforces §10.3 / chk_anonymous_actor:
//   - anonymous ⇔ no id (AL-D11: a claimed identity is never promoted to actor_id);
//   - every other type carries a UUID id;
//   - iam_system carries exactly the reserved sentinel.
func (a ActorRef) Validate() error {
	switch a.Type {
	case ActorAnonymous:
		if a.ID != "" {
			return NewError(ErrInvalidActor, "anonymous actor must not carry an id (AL-D11)")
		}
		return nil
	case ActorUser, ActorServiceAccount, ActorIAMSystem:
		if a.ID == "" {
			return NewError(ErrInvalidActor, "actor.id is required unless actor.type is anonymous")
		}
		if !IsUUID(a.ID) {
			return NewError(ErrInvalidRequest, "actor.id must be a UUID")
		}
		if a.Type == ActorIAMSystem && a.ID != IAMSystemActorID {
			return NewError(ErrInvalidActor, "iam_system actor must use the reserved id "+IAMSystemActorID)
		}
		return nil
	default:
		return NewError(ErrInvalidRequest, "actor.type must be one of user|service_account|iam_system|anonymous")
	}
}
