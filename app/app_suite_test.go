package app_test

import (
	"io/ioutil"
	"log"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestApp(t *testing.T) {
	RegisterFailHandler(Fail)
	log.SetOutput(ioutil.Discard)
	RunSpecs(t, "App Suite")
}
