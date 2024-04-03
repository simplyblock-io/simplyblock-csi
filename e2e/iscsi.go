package e2e

import (
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"k8s.io/kubernetes/test/e2e/framework"
)

// var _ = ginkgo.BeforeSuite(func() {
// 	deployCachenode()
// 	checkCachingNodes()
// })

var _ = ginkgo.Describe("SPDKCSI-ISCSI", func() {
	f := framework.NewDefaultFramework("spdkcsi")
	ginkgo.BeforeEach(func() {
		deployConfigs()
		deployCsi()

		// //deployCachenode()
		// err := checkCachingNodes(5 * time.Minute)
		// if err != nil {
		// 	ginkgo.Fail(err.Error())
		// }
	})

	ginkgo.AfterEach(func() {
		deleteCsi()
		deleteConfigs()
	})

	ginkgo.Context("Test SPDK CSI ISCSI", func() {
		ginkgo.It("Test SPDK CSI ISCSI", func() {
			ginkgo.By("checking controller statefulset is running", func() {
				err := waitForControllerReady(f.ClientSet, 4*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			ginkgo.By("checking node daemonset is running", func() {
				err := waitForNodeServerReady(f.ClientSet, 3*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			ginkgo.By("create a PVC and verify dynamic PV", func() {
				deployPVC()
				defer deletePVC()
				err := verifyDynamicPVCreation(f.ClientSet, "spdkcsi-pvc", 5*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			ginkgo.By("create a PVC and bind it to a pod", func() {
				deployPVC()
				deployTestPod()
				defer deletePVCAndTestPod()
				err := waitForTestPodReady(f.ClientSet, 3*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			ginkgo.By("create a Cache PVC and bind it to a cache pod", func() {
				deployCachePVC()
				deployCacheTestPod()
				defer deleteCachePVCAndCacheTestPod()
				err := waitForCacheTestPodReady(f.ClientSet, 3*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			ginkgo.By("check data persistency after the pod is removed and recreated", func() {
				deployPVC()
				deployTestPod()
				defer deletePVCAndTestPod()

				err := waitForTestPodReady(f.ClientSet, 3*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}

				err = checkDataPersist(f)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			///////////////////////////////////////
			ginkgo.By("create multiple pvcs and a pod with multiple pvcs attached, and check data persistence after the pod is removed and recreated", func() {
				deployMultiPvcs()
				deployTestPodWithMultiPvcs()
				defer func() {
					deleteMultiPvcsAndTestPodWithMultiPvcs()
					if err := waitForTestPodGone(f.ClientSet); err != nil {
						ginkgo.Fail(err.Error())
					}
					for _, pvcName := range []string{"spdkcsi-pvc1", "spdkcsi-pvc2", "spdkcsi-pvc3"} {
						if err := waitForPvcGone(f.ClientSet, pvcName); err != nil {
							ginkgo.Fail(err.Error())
						}
					}
				}()
				err := waitForTestPodReady(f.ClientSet, 3*time.Minute)
				if err != nil {
					ginkgo.Fail(err.Error())
				}

				/* 				ginkgo.By("restart csi driver", func() {
					//rolloutNodeServer()
					//rolloutControllerServer()
					err = waitForNodeServerReady(f.ClientSet, 3*time.Minute)
					if err != nil {
						ginkgo.Fail(err.Error())
					}
					err = waitForControllerReady(f.ClientSet, 4*time.Minute)
					if err != nil {
						ginkgo.Fail(err.Error())
					}
				}) */

				err = checkDataPersistForMultiPvcs(f)
				if err != nil {
					ginkgo.Fail(err.Error())
				}
			})

			//////////////////////////////////////
		})
	})
})
