workflow "agent" {

  node webhook "webhook_trigger" {
    method = "POST"
    path   = "/webhook"
  }

  node aggregate "aggregate_1" {
    items = "${webhook_trigger.output}"
  }

  node sort "sort_1" {
    key = "name"
  }

  node http "http_1" {
    url    = "https://example.com"
    method = "GET"
  }

  edge "webhook_trigger" "aggregate_1" {}
  edge "aggregate_1" "sort_1" {}
  edge "sort_1" "http_1" {}
}
