workflow "hello" {
  node webhook "trigger" {
    path = "/hello"
    method = "POST"
  }

  node http "greet" {
    url = "https://httpbin.org/get"
    method = "GET"
  }

  edge "trigger" "greet" {
  }
}
