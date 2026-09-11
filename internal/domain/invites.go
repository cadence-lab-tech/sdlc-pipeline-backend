package domain

import (
	"time"

	"github.com/google/uuid"
)

// Приглашение ссылкой (решение 0035): отложенное добавление в команду или
// проект, где участником становится тот, кто вошёл и предъявил ссылку.

type InviteTargetKind string

const (
	InviteTeam    InviteTargetKind = "team"
	InviteProject InviteTargetKind = "project"
)

type InviteStatus string

const (
	InvitePending  InviteStatus = "pending"
	InviteAccepted InviteStatus = "accepted"
	InviteExpired  InviteStatus = "expired"
	InviteRevoked  InviteStatus = "revoked"
)

// Invite — приглашение без токена и его хеша: токен виден один раз в ответе
// на выдачу, хеш не покидает базу.
type Invite struct {
	ID         uuid.UUID        `json:"id"`
	TargetKind InviteTargetKind `json:"target_kind"`
	TeamKey    *string          `json:"team_key,omitempty"`
	ProjectKey *string          `json:"project_key,omitempty"`
	TargetName string           `json:"target_name"`
	// У командного — роль в команде, у проектного — уровень и роли
	// специалистов (nil — любые, решение 0030).
	TeamRole *TeamRole   `json:"team_role,omitempty"`
	Level    *MemberRole `json:"level,omitempty"`
	RoleKeys []string    `json:"role_keys"`
	// Вычисляется из времён на момент чтения: колонка разошлась бы со
	// временем в первую же минуту после срока.
	Status           InviteStatus `json:"status"`
	CreatedByHandle  string       `json:"created_by_handle"`
	CreatedAt        time.Time    `json:"created_at"`
	ExpiresAt        time.Time    `json:"expires_at"`
	AcceptedByHandle *string      `json:"accepted_by_handle,omitempty"`
	AcceptedAt       *time.Time   `json:"accepted_at,omitempty"`
	RevokedByHandle  *string      `json:"revoked_by_handle,omitempty"`
	RevokedAt        *time.Time   `json:"revoked_at,omitempty"`

	TeamID    *uuid.UUID `json:"-"`
	ProjectID *uuid.UUID `json:"-"`
	CreatedBy uuid.UUID  `json:"-"`
}

// StatusAt — статус приглашения на момент now. Конечные состояния важнее
// срока: принятое после срока приглашение остаётся принятым.
func (i Invite) StatusAt(now time.Time) InviteStatus {
	switch {
	case i.RevokedAt != nil:
		return InviteRevoked
	case i.AcceptedAt != nil:
		return InviteAccepted
	case !now.Before(i.ExpiresAt):
		return InviteExpired
	}
	return InvitePending
}
