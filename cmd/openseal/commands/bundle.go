package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
)

func bundleCmd(args []string) {
	if err := runBundleCommand(os.Stdout, args, os.ReadFile); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: %v\n", err)
	}
}

type bundleFileReader func(string) ([]byte, error)

func runBundleCommand(output io.Writer, args []string, readFile bundleFileReader) error {
	if output == nil || readFile == nil {
		return errors.New("bundle output and file reader are required")
	}
	usage := "usage: openseal bundle validate <path> | inspect <path> | diff <from-path> <to-path> | plan-upgrade <current-path> <target-path>"
	if len(args) < 2 {
		return errors.New(usage)
	}
	load := func(path string) (*kernelbundle.Bundle, error) {
		data, err := readFile(path)
		if err != nil {
			return nil, err
		}
		return kernelbundle.DecodeYAML(data)
	}
	left, err := load(args[1])
	if err != nil {
		return err
	}
	var result interface{}
	switch args[0] {
	case "validate":
		if len(args) != 2 {
			return errors.New(usage)
		}
		verification, err := kernelbundle.Verify(left, kernelbundle.TrustPolicy{})
		if err != nil {
			return err
		}
		result = struct {
			Valid        bool     `json:"valid"`
			Digest       string   `json:"digest"`
			SignatureIDs []string `json:"signatureIds,omitempty"`
		}{Valid: true, Digest: verification.Digest, SignatureIDs: verification.ValidSignatureKeys}
	case "inspect":
		if len(args) != 2 {
			return errors.New(usage)
		}
		result, err = kernelbundle.Inspect(left)
		if err != nil {
			return err
		}
	case "diff", "plan-upgrade":
		if len(args) != 3 {
			return errors.New(usage)
		}
		right, err := load(args[2])
		if err != nil {
			return err
		}
		if args[0] == "diff" {
			result, err = kernelbundle.Compare(left, right)
		} else {
			result, err = kernelbundle.PlanUpgrade(left, right)
		}
		if err != nil {
			return err
		}
	default:
		return errors.New(usage)
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
