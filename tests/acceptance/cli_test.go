package acceptance_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("CLI Foundation", func() {
	Describe("shiftlog with no arguments", func() {
		It("displays help text", func() {
			stdout, _, err := testutil.RunShiftlog()
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Usage:"))
			Expect(stdout).To(ContainSubstring("Commands for humans:"))
		})
	})

	Describe("shiftlog --version", func() {
		It("displays the version number", func() {
			stdout, _, err := testutil.RunShiftlog("--version")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("shiftlog version"))
		})
	})

	Describe("shiftlog --help", func() {
		It("displays help text", func() {
			stdout, _, err := testutil.RunShiftlog("--help")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Usage:"))
			Expect(stdout).To(ContainSubstring("init"))
			Expect(stdout).To(ContainSubstring("store"))
			Expect(stdout).To(ContainSubstring("sync"))
		})
	})
})
