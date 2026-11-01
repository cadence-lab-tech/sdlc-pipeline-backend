package workflow

// Документы версии роли (решение 0068, пп. 1, 4.1, 5.1). Версия роли —
// упорядоченный набор документов; колонка role_versions.instructions —
// их склейка для clab, раннера и реплик прежних версий.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// RoleDocumentInput — документ заводимой версии; позиция — порядок в
// массиве, пустой kind — instructions.
type RoleDocumentInput struct {
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content"`
	// Instruction — документ ссылкой на версию инструкции библиотеки
	// (решение 0069): имя <ключ>.md, тип и байты берёт сервер, content
	// пуст. Оба способа у одного документа — отказ.
	Instruction *InstructionRef `json:"instruction,omitempty"`
	// Unpin — не переносить закрепление прежней версии на этот документ,
	// даже если текст тот же: «Отвязать» в форме роли. Клиент без
	// библиотеки поля не шлёт, и перенос работает (решение 0069, п. 4).
	Unpin bool `json:"unpin,omitempty"`

	// matchContent — ссылка из связки: версия ищется по sha содержимого, а
	// не по номеру (номера в связке справочные).
	matchContent bool
	// pin — разрешённое закрепление; ставит prepareDocuments.
	pin *domain.RoleDocumentPin
}

// InstructionRef — ключ и номер версии инструкции.
type InstructionRef struct {
	Key     string `json:"key"`
	Version int    `json:"version,omitempty"`
}

// MaxRoleDocuments — предохранитель, а не квота: роли CLAB разбиваются на
// 5–10 документов (решение 0068, п. 1.6).
const MaxRoleDocuments = 50

// roleDocumentName — форма имени файла: документ ложится файлом в каталог
// роли и в --instructions-dir. Нижний регистр, без «/» и ведущей точки, не
// длиннее 64 символов (1 + до 60 + «.md»). Та же форма — CHECK в 000038.
var roleDocumentName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,60}\.md$`)

// roleDocumentKind — форма типа. Список значений не закрыт: тип — данные,
// движок и захват по нему не ветвятся.
var roleDocumentKind = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// errEmptyInstructions — прежний текст отказа на пустую инструкцию: его ждут
// клиенты и формы прежних версий.
const errEmptyInstructions = "инструкция роли не может быть пустой"

// roleDocuments сводит вход версии к набору документов и проверяет его.
// instructions строкой без documents — один main.md; оба поля — отказ:
// какому верить, сервер не угадывает. limit — предел содержимого
// (Store.BlobLimit), 0 — нет.
func roleDocuments(in RoleVersionInput, limit int) ([]RoleDocumentInput, error) {
	if in.Documents != nil && in.Instructions != "" {
		return nil, apperr.InvalidInput("у версии роли либо instructions, либо documents, а не оба сразу")
	}
	if in.Documents == nil {
		if strings.TrimSpace(in.Instructions) == "" {
			return nil, apperr.InvalidInput(errEmptyInstructions)
		}
		docs := []RoleDocumentInput{{Name: domain.MainRoleDocument, Content: in.Instructions}}
		return docs, checkRoleDocuments(docs, limit)
	}
	docs := make([]RoleDocumentInput, len(in.Documents))
	copy(docs, in.Documents)
	return docs, checkRoleDocuments(docs, limit)
}

// checkRoleDocuments — все отказы набора (решение 0068, п. 1.6): сообщение
// называет документ номером с единицы и именем. Пустой kind становится
// instructions на месте.
func checkRoleDocuments(docs []RoleDocumentInput, limit int) error {
	if len(docs) == 0 {
		return apperr.InvalidInput("в версии роли нет ни одного документа")
	}
	if len(docs) > MaxRoleDocuments {
		return apperr.InvalidInput("в версии роли %d документов, предел %d", len(docs), MaxRoleDocuments)
	}
	seen := map[string]int{}
	for i := range docs {
		d := &docs[i]
		n := i + 1
		if d.Name == "" {
			return apperr.InvalidInput("документ %d: пустое имя", n)
		}
		if !roleDocumentName.MatchString(d.Name) {
			return apperr.InvalidInput(
				"документ %d (%q): имя — строчные латинские буквы, цифры, «.», «_», «-», "+
					"начинается с буквы или цифры, кончается на .md и не длиннее 64 символов", n, d.Name)
		}
		if first, dup := seen[d.Name]; dup {
			return apperr.InvalidInput("документ %d (%q): имя уже занято документом %d", n, d.Name, first)
		}
		seen[d.Name] = n
		if d.Kind == "" {
			d.Kind = domain.DefaultRoleDocumentKind
		}
		if !roleDocumentKind.MatchString(d.Kind) {
			return apperr.InvalidInput(
				"документ %d (%q): тип %q — строчные латинские буквы, цифры, «_», «-», "+
					"начинается с буквы, не длиннее 32 символов", n, d.Name, d.Kind)
		}
		if strings.TrimSpace(d.Content) == "" {
			return apperr.InvalidInput("документ %d (%q): пустое содержимое", n, d.Name)
		}
		// Склейка ложится в колонку text: NUL и невалидный UTF-8 Postgres в
		// text не примет, и вместо 400 вышла бы 500.
		if strings.IndexByte(d.Content, 0) >= 0 {
			return apperr.InvalidInput("документ %d (%q): содержимое с нулевым байтом", n, d.Name)
		}
		if !utf8.ValidString(d.Content) {
			return apperr.InvalidInput("документ %d (%q): содержимое — не UTF-8", n, d.Name)
		}
		if limit > 0 && len(d.Content) > limit {
			return apperr.InvalidInput("документ %d (%q): содержимое %d байт больше предела %d (SDLC_BLOB_MAX_BYTES)",
				n, d.Name, len(d.Content), limit)
		}
	}
	return nil
}

// ConcatDocuments — склейка набора в одну строку (решение 0068, п. 4.1):
// один документ — его содержимое байт в байт; несколько — по порядку
// маркер «<!-- document: имя -->», перевод строки, содержимое с переводом
// строки на конце (добавленным, если его нет), между документами пустая
// строка. Детерминирована: тот же набор — те же байты. Ею пишется колонка
// instructions; захват (CLAB-344) склеит ею набор этапа.
func ConcatDocuments(docs []domain.RoleDocument) string {
	if len(docs) == 1 {
		return content(docs[0])
	}
	var b strings.Builder
	for i, d := range docs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("<!-- document: ")
		b.WriteString(d.Name)
		b.WriteString(" -->\n")
		text := content(d)
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func content(d domain.RoleDocument) string {
	if d.Content == nil {
		return ""
	}
	return *d.Content
}

// documentsOf — набор входа в виде документов домена: для склейки и для
// сравнения с последней версией на импорте.
func documentsOf(docs []RoleDocumentInput) []domain.RoleDocument {
	out := make([]domain.RoleDocument, len(docs))
	for i, d := range docs {
		text := d.Content
		sum := sha256.Sum256([]byte(d.Content))
		out[i] = domain.RoleDocument{
			Name: d.Name, Kind: d.Kind, Position: i,
			SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(d.Content)), Content: &text,
			Instruction: d.pin,
		}
	}
	return out
}

func storeDocuments(docs []RoleDocumentInput) []postgres.RoleDocumentInput {
	out := make([]postgres.RoleDocumentInput, len(docs))
	for i, d := range docs {
		out[i] = postgres.RoleDocumentInput{Name: d.Name, Kind: d.Kind, Content: []byte(d.Content)}
		if d.pin != nil {
			id := d.pin.ID
			out[i].InstructionVersionID = &id
		}
	}
	return out
}

// sameDocuments — тот же набор по (name, kind, position, sha256)
// (решение 0068, п. 5.5) и закреплению (решение 0069): документ, ставший
// закреплённым при тех же байтах, — новая версия роли.
func sameDocuments(have, want []domain.RoleDocument) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		h, w := have[i], want[i]
		if h.Name != w.Name || h.Kind != w.Kind || h.Position != w.Position || h.SHA256 != w.SHA256 ||
			!samePin(h.Instruction, w.Instruction) {
			return false
		}
	}
	return true
}

func samePin(a, b *domain.RoleDocumentPin) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Key == b.Key && a.Version == b.Version
}

// InstructionDocumentName — имя закреплённого документа: всегда <ключ>.md
// (решение 0069, п. 2). На нём держатся отбор документов на статусе и
// «обновить в ролях».
func InstructionDocumentName(key string) string { return key + ".md" }

// prepareDocuments сводит вход версии роли к набору документов с
// закреплениями (решения 0068 и 0069). Порядок важен: ссылки разрешаются в
// имя, тип и байты до общей проверки набора — дубль имени и предел размера
// ловятся одним правилом для обоих способов; перенос закрепления — после
// неё, по уже проверенному набору. roleID — uuid.Nil у роли, которой ещё
// нет: переносить не из чего.
func (a *Authoring) prepareDocuments(ctx context.Context, q postgres.Querier, projectID, roleID uuid.UUID, in RoleVersionInput) ([]RoleDocumentInput, error) {
	if in.Documents != nil && in.Instructions != "" {
		return nil, apperr.InvalidInput("у версии роли либо instructions, либо documents, а не оба сразу")
	}
	if in.Documents == nil {
		docs, err := roleDocuments(in, a.store.BlobLimit())
		if err != nil {
			return nil, err
		}
		return docs, a.carryPins(ctx, q, roleID, docs)
	}
	docs := make([]RoleDocumentInput, len(in.Documents))
	copy(docs, in.Documents)
	for i := range docs {
		if docs[i].Instruction == nil {
			continue
		}
		if err := a.resolvePin(ctx, q, projectID, i+1, &docs[i]); err != nil {
			return nil, err
		}
	}
	if err := checkRoleDocuments(docs, a.store.BlobLimit()); err != nil {
		return nil, err
	}
	return docs, a.carryPins(ctx, q, roleID, docs)
}

// resolvePin разрешает ссылку документа n на версию инструкции проекта.
// Ссылка номером (REST, MCP, форма) — без содержимого; ссылка из связки
// (matchContent) — с содержимым, версия ищется по его sha.
func (a *Authoring) resolvePin(ctx context.Context, q postgres.Querier, projectID uuid.UUID, n int, d *RoleDocumentInput) error {
	ref := d.Instruction
	fail := func(format string, args ...any) error {
		return apperr.InvalidInput("документ %d: "+format, append([]any{n}, args...)...).
			WithDetails(map[string]any{"document": n, "instruction": ref.Key})
	}
	if strings.TrimSpace(ref.Key) == "" {
		return fail("у ссылки на инструкцию нет ключа (instruction.key)")
	}
	name := InstructionDocumentName(ref.Key)
	if d.Name != "" && d.Name != name {
		return fail("закреплённый за инструкцией %s документ зовётся %s, а не %q", ref.Key, name, d.Name)
	}
	inst, err := a.store.GetInstructionByKey(ctx, q, projectID, ref.Key)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return fail("инструкции %s нет в проекте", ref.Key)
	}
	if err != nil {
		return err
	}
	var v domain.InstructionVersion
	if d.matchContent {
		v, err = a.store.FindInstructionVersionBySHA(ctx, q, inst.ID, sha256Hex(d.Content))
		if apperr.As(err).Code == apperr.CodeNotFound {
			return fail("%s: текст не совпадает ни с одной версией инструкции %s", name, ref.Key)
		}
		if err != nil {
			return err
		}
	} else {
		if d.Content != "" {
			return fail("у документа либо content, либо instruction, а не оба сразу")
		}
		if ref.Version < 1 {
			return fail("у ссылки на инструкцию %s нет номера версии (instruction.version)", ref.Key)
		}
		v, err = a.store.GetInstructionVersion(ctx, q, inst.ID, ref.Version, true)
		if apperr.As(err).Code == apperr.CodeNotFound {
			return fail("версии %d инструкции %s нет", ref.Version, ref.Key)
		}
		if err != nil {
			return err
		}
		d.Content = deref(v.Content)
	}
	if d.Kind != "" && d.Kind != v.Kind {
		return fail("тип закреплённого документа — тип версии инструкции (%s), а не %q", v.Kind, d.Kind)
	}
	d.Name, d.Kind = name, v.Kind
	d.pin = &domain.RoleDocumentPin{ID: v.ID, Key: ref.Key, Version: v.Version}
	return nil
}

// carryPins — перенос закрепления (решение 0069, п. 4): документ пришёл
// содержимым, а в последней версии роли под тем же именем лежал
// закреплённый документ с теми же типом и байтами — закрепление переходит в
// новую версию. Так клиент, не знающий библиотеки, теряет закрепление
// только у документа, который и правда изменил. Unpin — явный отказ.
func (a *Authoring) carryPins(ctx context.Context, q postgres.Querier, roleID uuid.UUID, docs []RoleDocumentInput) error {
	if roleID == uuid.Nil {
		return nil
	}
	latest, err := a.store.LatestRoleVersion(ctx, q, roleID)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	have := []domain.RoleVersion{latest}
	if err := a.store.AttachRoleDocuments(ctx, q, have, false); err != nil {
		return err
	}
	pinned := map[string]domain.RoleDocument{}
	for _, d := range have[0].Documents {
		if d.Instruction != nil {
			pinned[d.Name] = d
		}
	}
	if len(pinned) == 0 {
		return nil
	}
	for i := range docs {
		d := &docs[i]
		if d.pin != nil || d.Unpin {
			continue
		}
		prev, ok := pinned[d.Name]
		if ok && prev.Kind == d.Kind && prev.SHA256 == sha256Hex(d.Content) {
			pin := *prev.Instruction
			d.pin = &pin
		}
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
