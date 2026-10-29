package domain

import (
	"time"

	"github.com/google/uuid"
)

// FeedEvent — событие ленты в ответе ручек долгого опроса (CLAB-323, решение
// 0067 п. 4). Без payload: событие только будит клиента, а подробности и
// истина — карточка задачи и /timeline. Поля добавляются только дополнением:
// их читают clab wait и плагин (CLAB-329).
type FeedEvent struct {
	ID      uuid.UUID `json:"id"`
	Type    string    `json:"type"`
	TaskKey string    `json:"task_key"`
	// Хэндл действующего лица; null у события без актора.
	Actor     *string   `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// FeedMark — след события для будильника ожидающих: какой задаче и какому
// проекту оно принадлежит. Ни типа, ни актора будильнику не нужно.
type FeedMark struct {
	ID        uuid.UUID
	TaskID    uuid.UUID
	ProjectID uuid.UUID
}
