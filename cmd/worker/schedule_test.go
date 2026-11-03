package main

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github/githubtest"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Периоды заданий (CLAB-395, AC-10, AC-11): синхронизация PR идёт своим
// тикером, опрос установок — только с приложением, выключенная индексация
// выключает и очередь, и PR.
func TestScheduleFollowsConfig(t *testing.T) {
	cfg := config.Config{
		LeaseSweepInterval:       time.Minute,
		IndexEnabled:             true,
		IndexInterval:            10 * time.Minute,
		IndexPollInterval:        15 * time.Second,
		RepoRefSyncInterval:      time.Minute,
		InstallationSyncInterval: 2 * time.Minute,
	}
	s := newSchedule(cfg, true)
	if s.Index != 10*time.Minute || s.Queue != 15*time.Second ||
		s.RepoRefSync != time.Minute || s.InstallationSync != 2*time.Minute {
		t.Fatalf("периоды: %+v", s)
	}
	if s := newSchedule(cfg, false); s.InstallationSync != 0 {
		t.Fatalf("опрос установок без приложения: %s", s.InstallationSync)
	}
	cfg.IndexEnabled = false
	s = newSchedule(cfg, true)
	if s.Index != 0 || s.Queue != 0 || s.RepoRefSync != 0 {
		t.Fatalf("выключенная индексация оставила задания: %+v", s)
	}
	if s.InstallationSync != 2*time.Minute || s.LeaseSweep != time.Minute {
		t.Fatalf("опрос установок и аренды зависят от индексации: %+v", s)
	}
	if c, stop := s.ticker(0); c != nil {
		stop()
		t.Fatal("нулевой период завёл тикер")
	}
	c, stop := s.ticker(time.Millisecond)
	defer stop()
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("тикер не тикает")
	}
}

// AC-9, AC-10: задание installation_sync в метриках под своим именем —
// метка успеха после прохода, счётчик ошибок на сбое; без прохода метки
// успеха нет.
func TestInstallationSyncReachesMetrics(t *testing.T) {
	if !slices.Contains(workerJobs, "installation_sync") {
		t.Fatalf("installation_sync нет в workerJobs: %v", workerJobs)
	}
	store, _ := testdb.Store(t)
	ctx := context.Background()
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{})
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	who, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 651, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: *project.OwnerTeamID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gh := githubtest.New(t)
	gh.Installations[651] = &githubtest.Installation{}
	provider := gh.Provider(t)

	reg := metrics.New("worker", "dev")
	m := metrics.NewWorker(reg, workerJobs...)
	const success = `cadence_worker_job_last_success_timestamp_seconds{job="installation_sync"}`
	const errorsTotal = `cadence_worker_job_errors_total{job="installation_sync"}`
	if v := scrapeValue(t, reg, success); v != -1 {
		t.Fatalf("метка успеха до прохода: %v", v)
	}
	if v := scrapeValue(t, reg, errorsTotal); v != 0 {
		t.Fatalf("ошибок до прохода: %v", v)
	}

	syncInstallations(ctx, engine, provider, log, m)
	if v := scrapeValue(t, reg, success); v <= 0 {
		t.Fatalf("метка успеха после прохода: %v", v)
	}
	gh.Fail[githubtest.KindList] = http.StatusBadGateway
	syncInstallations(ctx, engine, provider, log, m)
	if v := scrapeValue(t, reg, errorsTotal); v != 1 {
		t.Fatalf("ошибок после сбоя: %v", v)
	}
}
