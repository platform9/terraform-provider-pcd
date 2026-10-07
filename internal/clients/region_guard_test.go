// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every resource and data source with a region attribute must build its
// service clients for that region. A client built straight from the
// provider's configuration reaches the provider's region whatever the
// resource's region says, which is how region came to be ignored. This scans
// the services for a call like config.NetworkV2Client() that does not go
// through ForRegion, in each file that declares a region attribute.
func TestRegionalServicesBuildClientsForTheirRegion(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "services", "*", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("listing the services: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if !strings.Contains(string(src), `"region"`) {
			continue
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(sel.Sel.Name, "Client") {
				return true
			}
			// The receiver of a direct call is the config field itself:
			// r.config.NetworkV2Client(). Through ForRegion it is a call:
			// r.config.ForRegion(region).NetworkV2Client().
			if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "config" {
				t.Errorf("%s: %s is built for the provider's region, not the resource's; use ForRegion",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked < 60 {
		t.Fatalf("checked %d files with a region attribute; the scan is not finding the services", checked)
	}
}
