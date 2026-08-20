package initcmd

import (
	"fmt"
	"os"
	"path/filepath"
)

const config = `project {
  base_dir = "./tracetests"
  output   = "pretty"
}
`

const test = `variable "BASE_URL" {
  type    = string
  default = "http://localhost:8080"
}

test "healthcheck" {
  tags = ["smoke"]

  step "health" {
    http_request {
      method = "GET"
      url    = "${var.BASE_URL}/health"
    }

    check "healthy" {
      response = {
        "status code" = response.status_code == 200
      }
    }
  }
}
`

func Run(dir string) error {
	configPath := filepath.Join(dir, ".tracetest.hcl")
	baseDir := filepath.Join(dir, "tracetests")
	testPath := filepath.Join(baseDir, "healthcheck.hcl")
	for _, path := range []string{configPath, testPath} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing file %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(testPath, []byte(test), 0o644); err != nil {
		return err
	}
	fmt.Printf("Initialized Tracetest project. Run: tracetest run\n")
	return nil
}
