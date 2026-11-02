package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Правило причин внимания на фактах, без базы (CLAB-363, AC-A10…A17, A27):
// каждая причина по своему факту, порядок важности, фильтры на множествах и
// расширение реестра одной записью.

var attentionNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func ptrTime(t time.Time) *time.Time { return &t }

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

func attentionRun(state string, attempt int) *domain.StageRun {
	role := uuid.New()
	return &domain.StageRun{ID: uuid.New(), Kind: domain.StageKindStage, State: state, Attempt: attempt,
		RoleID: &role, RoleKey: "developer", EnteredAt: attentionNow.Add(-3 * time.Hour)}
}

func codesOf(f AttentionFacts) []string { return Codes(EvalAttention(f)) }

func TestAttentionEachRule(t *testing.T) {
	holder := "agent:bot"
	live := attentionRun(domain.StageClaimed, 1)
	live.ClaimedBy, live.ClaimedHandle = ptrUUID(uuid.New()), &holder
	live.LeaseExpires = ptrTime(attentionNow.Add(time.Minute))

	stale := attentionRun(domain.StageInProgress, 1)
	stale.ClaimedBy, stale.ClaimedHandle = ptrUUID(uuid.New()), &holder
	stale.LeaseExpires = ptrTime(attentionNow.Add(-time.Minute))

	pending := attentionRun(domain.StagePending, 1)
	noRole := attentionRun(domain.StagePending, 1)
	noRole.RoleID = nil
	returned := attentionRun(domain.StageClaimed, 2)
	returned.ClaimedBy, returned.LeaseExpires = ptrUUID(uuid.New()), ptrTime(attentionNow.Add(time.Hour))
	rejectedFrom := attentionRun(domain.StagePending, 1)
	rejectedFrom.RejectedFromRunID = ptrUUID(uuid.New())

	limit := &StageLimit{Gate: "time", MaxDuration: "1h", Deadline: attentionNow.Add(-time.Hour), Overdue: true}
	waived := *limit
	waived.Waived = true
	notYet := &StageLimit{Gate: "time", MaxDuration: "4h", Deadline: attentionNow.Add(time.Hour)}

	expired := &postgres.LeaseEvent{Type: domain.EventLeaseExpired, At: attentionNow.Add(-time.Hour), Holder: &holder}
	reclaimed := &postgres.LeaseEvent{Type: domain.EventStageClaimed, At: attentionNow}
	released := &postgres.LeaseEvent{Type: domain.EventStageReleased, At: attentionNow}

	for _, c := range []struct {
		name string
		f    AttentionFacts
		want []string
	}{
		{"живая аренда — причин нет", AttentionFacts{Stage: live, Current: live}, nil},
		{"ждёт человека", AttentionFacts{Stage: live, Current: live,
			HumanGates: []AttentionGate{{"analysis_done", "human_approval"}}}, []string{AttentionAwaitsHuman}},
		{"отклонено", AttentionFacts{Stage: live, Current: live,
			Rejected: []RejectedGate{{Transition: "analysis_done", Gate: "human_approval", DecidedAt: attentionNow}}},
			[]string{AttentionGateRejected}},
		// Аренда вышла, воркер не прошёл: и протухание, и «никто не держит».
		{"срок аренды вышел", AttentionFacts{Stage: stale, Current: stale}, []string{AttentionLeaseExpired, AttentionUnclaimed}},
		{"воркер вернул в pending", AttentionFacts{Stage: pending, Current: pending, LeaseEvent: expired},
			[]string{AttentionLeaseExpired, AttentionUnclaimed}},
		{"захвачен заново после протухания", AttentionFacts{Stage: pending, Current: pending, LeaseEvent: reclaimed},
			[]string{AttentionUnclaimed}},
		{"явный отпуск", AttentionFacts{Stage: pending, Current: pending, LeaseEvent: released}, []string{AttentionUnclaimed}},
		{"предел вышел", AttentionFacts{Stage: live, Current: live, Limit: limit}, []string{AttentionOverStageLimit}},
		{"предел снят", AttentionFacts{Stage: live, Current: live, Limit: &waived}, nil},
		{"предел не вышел", AttentionFacts{Stage: live, Current: live, Limit: notYet}, nil},
		{"блокирующие", AttentionFacts{Stage: live, Current: live, BlockedBy: []string{"X-1"}}, []string{AttentionBlocked}},
		{"возврат: попытка 2", AttentionFacts{Stage: returned, Current: returned}, []string{AttentionReturned}},
		{"возврат: rejected_from", AttentionFacts{Stage: rejectedFrom, Current: rejectedFrom},
			[]string{AttentionReturned, AttentionUnclaimed}},
		{"не забран", AttentionFacts{Stage: pending, Current: pending}, []string{AttentionUnclaimed}},
		{"статус без роли", AttentionFacts{Stage: noRole, Current: noRole}, nil},
		{"нет прогона", AttentionFacts{}, nil},
	} {
		f := c.f
		f.Now = attentionNow
		if got := codesOf(f); !slices.Equal(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%s: %v, ждали %v", c.name, got, c.want)
		}
	}
}

// AC-A16: все причины сразу — в порядке важности, без повторов, у каждой
// code, since и поля из таблицы.
func TestAttentionOrderAndFields(t *testing.T) {
	holder := "agent:bot"
	stage := attentionRun(domain.StageGating, 3)
	review := attentionRun(domain.StageClaimed, 1)
	review.Kind = domain.StageKindReview
	review.ClaimedBy, review.ClaimedHandle = ptrUUID(uuid.New()), &holder
	review.LeaseExpires = ptrTime(attentionNow.Add(-time.Minute))
	reason := "нет критерия AC-3"
	f := AttentionFacts{
		Now: attentionNow, Stage: stage, Current: review,
		HumanGates: []AttentionGate{{"qa_passed", "human_approval"}},
		Rejected: []RejectedGate{
			{Transition: "a", Gate: "g2", DecidedAt: attentionNow.Add(-time.Minute)},
			{Transition: "a", Gate: "g1", Reason: &reason, DecidedAt: attentionNow.Add(-2 * time.Minute)},
		},
		Limit:     &StageLimit{Gate: "time", MaxDuration: "1h", Deadline: attentionNow.Add(-time.Hour), Overdue: true},
		BlockedBy: []string{"X-1", "X-2"},
	}
	got := EvalAttention(f)
	if want := AttentionCodes(); !slices.Equal(Codes(got), want) {
		t.Fatalf("порядок %v, ждали %v", Codes(got), want)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	fields := map[string][]string{
		AttentionAwaitsHuman:    {"gates"},
		AttentionGateRejected:   {"gates"},
		AttentionLeaseExpired:   {"holder", "run_kind"},
		AttentionOverStageLimit: {"max_duration", "gate"},
		AttentionReturned:       {"attempt"},
	}
	for _, it := range items {
		code := it["code"].(string)
		if _, ok := it["since"]; !ok {
			t.Errorf("%s: нет since: %v", code, it)
		}
		for _, k := range fields[code] {
			if _, ok := it[k]; !ok {
				t.Errorf("%s: нет поля %s: %v", code, k, it)
			}
		}
	}
	if items[4]["since"] != nil {
		t.Errorf("blocked: since должен быть null: %v", items[4])
	}
	if items[1]["since"] != attentionNow.Add(-2*time.Minute).Format(time.RFC3339) {
		t.Errorf("gate_rejected: since — первое отклонение: %v", items[1]["since"])
	}
	gates := items[1]["gates"].([]any)
	if g := gates[0].(map[string]any); g["reason"] != nil || g["gate"] != "g2" {
		t.Errorf("отклонение без причины — reason null: %v", g)
	}
	if g := gates[1].(map[string]any); g["reason"] != reason {
		t.Errorf("причина отклонения: %v", g)
	}
	if items[2]["holder"] != holder || items[2]["run_kind"] != domain.StageKindReview {
		t.Errorf("lease_expired перепроверки: %v", items[2])
	}
	if items[5]["attempt"] != float64(3) {
		t.Errorf("returned: %v", items[5])
	}
	if empty, _ := json.Marshal(EvalAttention(AttentionFacts{Now: attentionNow})); string(empty) != "[]" {
		t.Errorf("без причин — [], а не %s", empty)
	}
}

// AC-A17: фильтры на множествах кодов.
func TestAttentionFilterSets(t *testing.T) {
	parse := func(q map[string]string) AttentionFilter {
		t.Helper()
		f, err := ParseAttentionFilter(func(k string) string { return q[k] })
		if err != nil {
			t.Fatalf("%v: %v", q, err)
		}
		return f
	}
	none := []string{}
	onlyUnclaimed := []string{AttentionUnclaimed}
	retUnclaimed := []string{AttentionReturned, AttentionUnclaimed}
	stuck := []string{AttentionLeaseExpired, AttentionUnclaimed}
	human := []string{AttentionAwaitsHuman, AttentionUnclaimed}
	future := []string{"awaits_reply"}
	for _, c := range []struct {
		q    map[string]string
		in   [][]string
		out  [][]string
		note string
	}{
		{map[string]string{"attention": "true"}, [][]string{onlyUnclaimed, stuck}, [][]string{none}, "есть причина"},
		{map[string]string{"attention": "false"}, [][]string{none}, [][]string{onlyUnclaimed}, "нет причин"},
		{map[string]string{"attention": "blocked,lease_expired"}, [][]string{stuck}, [][]string{none, onlyUnclaimed}, "любая из"},
		{map[string]string{"attention_not": "awaits_human"}, [][]string{none, stuck}, [][]string{human}, "ни одной из"},
		{map[string]string{"attention_except": "awaits_human,returned,unclaimed"}, [][]string{stuck, future},
			[][]string{none, onlyUnclaimed, retUnclaimed, human}, "причина вне списка, будущий код — причина"},
		{map[string]string{"attention_within": "returned"}, [][]string{none, {AttentionReturned}}, [][]string{onlyUnclaimed}, "все в списке"},
		{map[string]string{"attention_within": "returned,unclaimed", "attention": "unclaimed"},
			[][]string{onlyUnclaimed, retUnclaimed}, [][]string{none, {AttentionReturned}, stuck}, "очередь по списку"},
		{map[string]string{"attention_not": "awaits_human", "attention_except": "awaits_human,returned,unclaimed"},
			[][]string{stuck}, [][]string{human, none, retUnclaimed}, "зона «Застряло»"},
	} {
		f := parse(c.q)
		if !f.Active() {
			t.Errorf("%s: фильтр не активен", c.note)
		}
		for _, codes := range c.in {
			if !f.Match(codes) {
				t.Errorf("%s: %v должен подходить", c.note, codes)
			}
		}
		for _, codes := range c.out {
			if f.Match(codes) {
				t.Errorf("%s: %v не должен подходить", c.note, codes)
			}
		}
	}
	if parse(map[string]string{}).Active() {
		t.Error("без параметров фильтр активен")
	}
	for _, bad := range []map[string]string{
		{"attention": "nope"}, {"attention_not": "awaits_human,nope"},
		{"attention_except": "x"}, {"attention_within": ","}, {"attention": "yes"},
	} {
		_, err := ParseAttentionFilter(func(k string) string { return bad[k] })
		if err == nil || !strings.Contains(err.Error(), AttentionUnclaimed) {
			t.Errorf("%v: ждали ошибку с перечнем известных, а не %v", bad, err)
		}
	}
}

// AC-A27: причина добавляется одной записью реестра — она в ответе, в
// известных кодах и в фильтрах, без правок разбора и обработчика.
func TestAttentionRegistryExtends(t *testing.T) {
	saved := slices.Clone(attentionRules)
	t.Cleanup(func() { attentionRules = saved })
	attentionRules = append(attentionRules, attentionRule{"test_reason", func(f AttentionFacts) (Attention, bool) {
		return Attention{Fields: map[string]any{"note": "x"}}, len(f.BlockedBy) > 1
	}})

	got := EvalAttention(AttentionFacts{Now: attentionNow, BlockedBy: []string{"A-1", "A-2"}})
	if !slices.Equal(Codes(got), []string{AttentionBlocked, "test_reason"}) {
		t.Fatalf("коды: %v", Codes(got))
	}
	if !slices.Contains(AttentionCodes(), "test_reason") {
		t.Fatal("кода нет в известных")
	}
	f, err := ParseAttentionFilter(func(k string) string {
		if k == "attention" {
			return "test_reason"
		}
		return ""
	})
	if err != nil || !f.Match(Codes(got)) {
		t.Fatalf("фильтр по новому коду: %v", err)
	}
	raw, _ := json.Marshal(got[1])
	var item map[string]any
	_ = json.Unmarshal(raw, &item)
	if item["code"] != "test_reason" || item["note"] != "x" {
		t.Fatalf("объект причины: %s", raw)
	}
	if _, ok := item["since"]; !ok {
		t.Fatalf("нет since: %s", raw)
	}
	// Коды не повторяются: тест держит реестр от второго кода с тем же именем.
	seen := map[string]bool{}
	for _, c := range AttentionCodes() {
		if seen[c] {
			t.Fatalf("код %q в реестре дважды", c)
		}
		seen[c] = true
	}
}

// Реестр без повторов кодов в рабочем виде.
func TestAttentionRegistryUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range AttentionCodes() {
		if seen[c] {
			t.Fatalf("код %q в реестре дважды", c)
		}
		seen[c] = true
	}
	if len(seen) != 7 {
		t.Fatalf("кодов %d", len(seen))
	}
}
