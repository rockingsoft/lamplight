variable "BASE_URL" {
  default = "https://example.test"
}

variable "TEMPO_ENDPOINT" {
  type = string
}

test "checkout" {
  tags = ["smoke", "checkout"]

  step "login" {
    http_request {
      method = "GET"
      url    = "${var.BASE_URL}/login"
    }

    outputs = {
      id = response.json.id
    }
  }

  step "order" {
    http_request {
      method = "POST"
      url    = "${var.BASE_URL}/orders/${steps.login.outputs.id}"
    }

    check "created" {
      response = {
        status = response.status_code == 201
      }
    }
  }
}
