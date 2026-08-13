// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import "testing"

// accountFromStatusLine is the parsing half of controllerAccount, exercised
// directly. A wrong answer here does not error — it silently compares against
// the wrong account, which is the failure this whole check exists to prevent.
func accountFromStatusLine(line string) string {
	return parseAccountLine(line)
}

func TestParseAccountLine(t *testing.T) {
	cases := map[string]struct{ line, want string }{
		"ownerAccountID preferred": {
			"025066259430\tarn:aws:iam::999999999999:role/x", "025066259430"},
		"falls back to the ARN": {
			"\tarn:aws:iam::025066259430:role/inference-demo-node-role", "025066259430"},
		"ec2 ARN carries the account in the same position": {
			"\tarn:aws:ec2:us-west-2:025066259430:vpc/vpc-0abc", "025066259430"},
		// A resource that never reached AWS has neither. Returning "" makes the
		// caller skip the check rather than compare against nothing and fail a
		// teardown for the wrong reason.
		"pending resource yields nothing":     {"\t", ""},
		"empty line yields nothing":           {"", ""},
		"malformed arn yields nothing":        {"\tnot-an-arn", ""},
		"short arn yields nothing":            {"\tarn:aws:iam", ""},
		"arn with empty account yields empty": {"\tarn:aws:iam:::role/x", ""},
	}
	for name, c := range cases {
		if got := accountFromStatusLine(c.line); got != c.want {
			t.Errorf("%s: got %q, want %q (line %q)", name, got, c.want, c.line)
		}
	}
}
