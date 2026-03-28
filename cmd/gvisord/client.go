// Copyright 2026 Shayon Mukherjee
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
)

// clientMain handles `gvisord execute`, `gvisord complete`, etc. when called
// with a subcommand.
func clientMain(method, socketPath string, args []string) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error connecting to %s: %v\n", socketPath, err)
		os.Exit(1)
	}
	defer conn.Close()

	type request struct {
		Method string      `json:"method"`
		Params interface{} `json:"params,omitempty"`
	}
	req := request{Method: method}

	switch method {
	case "run":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: gvisord run <workload> <json_params>\n")
			fmt.Fprintf(os.Stderr, "  e.g. gvisord run python '{\"script\":\"print(1)\"}'\n")
			os.Exit(1)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
			fmt.Fprintf(os.Stderr, "invalid JSON params: %v\n", err)
			os.Exit(1)
		}
		params["workload"] = args[0]
		req.Params = params
	case "execute":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "usage: gvisord execute <workload> [checkpoint]\n")
			os.Exit(1)
		}
		params := map[string]string{"workload": args[0]}
		if len(args) >= 2 {
			params["checkpoint"] = args[1]
		}
		req.Params = params
	case "complete":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "usage: gvisord complete <lease_id>\n")
			os.Exit(1)
		}
		req.Params = map[string]string{"lease_id": args[0]}
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "error sending request: %v\n", err)
		os.Exit(1)
	}

	var resp struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "error reading response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(resp.Result, "", "  ")
	fmt.Println(string(out))
}
