project {
  base_dir = "tests"
  output   = "json"
}

datasource "tempo" {
  endpoint = var.TEMPO_ENDPOINT
}
