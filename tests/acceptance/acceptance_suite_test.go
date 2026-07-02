package acceptance_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Acceptance Suite")
}

var _ = BeforeSuite(func() {
	Expect(testutil.BuildBinary()).To(Succeed())
})

var _ = AfterSuite(func() {
	testutil.CleanupBinary()
})
