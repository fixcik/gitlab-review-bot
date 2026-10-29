package sysmap

import "testing"

func TestParseSpecOpenAPI3(t *testing.T) {
	spec := `openapi: 3.0.3
info:
  title: Orders
  version: "1"
servers:
  - url: https://orders.example.com/api
paths:
  /orders:
    get:
      operationId: listOrders
    post:
      summary: Create an order
  /orders/{orderId}:
    parameters:
      - name: orderId
        in: path
    delete:
      operationId: deleteOrder
  /shared:
    $ref: './paths.yaml#/shared'
  x-internal: true
components:
  schemas:
    Order:
      $ref: './order.yaml'
`
	es, err := ParseSpec("api/openapi.yaml", []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		method, path string
		line         int
	}
	exp := []want{{"GET", "/orders", 9}, {"POST", "/orders", 11}, {"DELETE", "/orders/{orderId}", 17}, {"", "/shared", 19}}
	if len(es) != len(exp) {
		t.Fatalf("entries = %+v", es)
	}
	for i, w := range exp {
		e := es[i]
		if e.Method != w.method || e.Path != w.path || e.Line != w.line || e.Side != Provides || e.Protocol != HTTP || e.Source != SourceOpenAPI || e.File != "api/openapi.yaml" {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
	}
	if es[0].Note != "listOrders" || es[1].Note != "Create an order" {
		t.Errorf("notes: %q %q", es[0].Note, es[1].Note)
	}
}

func TestParseSpecSwagger2BasePathJSON(t *testing.T) {
	spec := `{
  "swagger": "2.0",
  "basePath": "/api/v1/",
  "paths": {
    "/users/{id}": {
      "get": {"operationId": "getUser"},
      "put": {}
    }
  }
}`
	es, err := ParseSpec("swagger.json", []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Path != "/api/v1/users/{id}" || es[0].Method != "GET" || es[1].Method != "PUT" {
		t.Fatalf("entries = %+v", es)
	}
	if es[0].Line != 6 {
		t.Errorf("line = %d, want 6", es[0].Line)
	}
}

func TestParseSpecRejectsNonSpec(t *testing.T) {
	if _, err := ParseSpec("x.yaml", []byte("name: app\n")); err == nil {
		t.Error("want error for a non-spec document")
	}
}
