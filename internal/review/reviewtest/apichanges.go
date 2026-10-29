package reviewtest

import (
	"github.com/fixcik/gitlab-review-bot/internal/compat"
)

// ApiOrders is the changelog of testdata/specs (internal/compat) as oasdiff
// 1.34.1 reports it with --lang ru.
var ApiOrders = compat.APIChanges{Spec: "api/openapi.yaml", Items: []compat.APIChange{
	{Level: 3, Rule: "request-property-became-required", Operation: "POST", Endpoint: "/orders", Text: "поле запроса `currency` стало обязательным", File: "api/openapi.yaml", Line: 14},
	{Level: 3, Rule: "response-property-type-changed", Operation: "POST", Endpoint: "/orders", Text: "у свойства ответа `total` `type` изменён с `number` на `string` для статуса `200`", File: "api/openapi.yaml", Line: 27},
	{Level: 3, Rule: "api-removed-without-deprecation", Operation: "DELETE", Endpoint: "/orders/{id}", Text: "API удалён без deprecation", File: "api/openapi.yaml"},
	{Level: 1, Rule: "new-optional-request-property", Operation: "POST", Endpoint: "/orders", Text: "добавлено новое необязательное поле запроса `comment`", File: "api/openapi.yaml", Line: 18},
	{Level: 1, Rule: "response-non-success-status-added", Operation: "GET", Endpoint: "/orders/{id}", Text: "добавлен ответ об отсутствии успеха со статусом `404`", File: "api/openapi.yaml", Line: 33},
	{Level: 1, Rule: "endpoint-added", Operation: "GET", Endpoint: "/orders/{id}/history", Text: "эндпоинт добавлен", File: "api/openapi.yaml", Line: 35},
	{Level: 2, Rule: "response-property-enum-value-added", Operation: "GET", Endpoint: "/orders/{id}", Text: "добавлено значение enum `x`", File: "api/schemas/order.yaml", Line: 7},
	{Level: 1, Rule: "api-schema-added", Text: "схема `Order` добавлена"},
}}
