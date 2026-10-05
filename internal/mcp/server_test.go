package mcp

import (
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// Отказ приложения инстанса (CLAB-78) уходит агенту тем же кодом, что и в
// REST, и с исходным текстом: по нему видно, что чинить не агенту.
func TestFailKeepsProviderMisconfigured(t *testing.T) {
	res, _, err := fail(apperr.New(apperr.CodeProviderMisconfigured, "приложение GitHub инстанса не принято провайдером"))
	if err != nil || !res.IsError {
		t.Fatalf("результат: %+v, %v", res, err)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "provider_misconfigured" ||
		body.Error.Message != "приложение GitHub инстанса не принято провайдером" {
		t.Fatalf("ошибка: %+v", body.Error)
	}
}
