package cli

import (
	"fmt"
	"github.com/mattfenwick/cyclonus/pkg/connectivity"
	"github.com/mattfenwick/cyclonus/pkg/connectivity/probe"
	"github.com/mattfenwick/cyclonus/pkg/generator"
	"github.com/mattfenwick/cyclonus/pkg/kube"
	"github.com/mattfenwick/cyclonus/pkg/utils"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"strings"
)

type GenerateArgs struct {
	AllowDNS                  bool
	Noisy                     bool
	IgnoreLoopback            bool
	PerturbationWaitSeconds   int
	PodCreationTimeoutSeconds int
	Retries                   int
	BatchJobs                 bool
	Context                   string
	ServerPorts               []int
	ServerProtocols           []string
	ServerNamespaces          []string
	ServerPods                []string
	CleanupNamespaces         bool
	Include                   []string
	Exclude                   []string
	DestinationType           string
	Mock                      bool
	DryRun                    bool
}

func SetupGenerateCommand() *cobra.Command {
	args := &GenerateArgs{}

	command := &cobra.Command{
		Use:   "generate",
		Short: "generate network policies",
		Long:  "generate network policies, create and probe against kubernetes, and compare to expected results",
		Args:  cobra.ExactArgs(0),
		Run: func(cmd *cobra.Command, as []string) {
			RunGenerateCommand(args)
		},
	}

	command.Flags().StringSliceVar(&args.ServerProtocols, "server-protocol", []string{"TCP", "UDP", "SCTP"}, "protocols to run server on")
	command.Flags().IntSliceVar(&args.ServerPorts, "server-port", []int{80, 81}, "ports to run server on")
	command.Flags().StringSliceVar(&args.ServerNamespaces, "namespace", []string{"x", "y", "z"}, "namespaces to create/use pods in")
	command.Flags().StringSliceVar(&args.ServerPods, "pod", []string{"a", "b", "c"}, "pods to create in namespaces")

	command.Flags().BoolVar(&args.BatchJobs, "batch-jobs", false, "if true, run jobs in batches to avoid saturating the Kube APIServer with too many exec requests")
	command.Flags().IntVar(&args.Retries, "retries", 1, "number of kube probe retries to allow, if probe fails")
	command.Flags().BoolVar(&args.AllowDNS, "allow-dns", true, "if using egress, allow udp over port 53 for DNS resolution")
	command.Flags().BoolVar(&args.Noisy, "noisy", false, "if true, print all results")
	command.Flags().BoolVar(&args.IgnoreLoopback, "ignore-loopback", false, "if true, ignore loopback for truthtable correctness verification")
	command.Flags().IntVar(&args.PerturbationWaitSeconds, "perturbation-wait-seconds", 5, "number of seconds to wait after perturbing the cluster (i.e. create a network policy, modify a ns/pod label) before running probes, to give the CNI time to update the cluster state")
	command.Flags().IntVar(&args.PodCreationTimeoutSeconds, "pod-creation-timeout-seconds", 60, "number of seconds to wait for pods to create, be running and have IP addresses")
	command.Flags().StringVar(&args.Context, "context", "", "kubernetes context to use; if empty, uses default context")
	command.Flags().BoolVar(&args.CleanupNamespaces, "cleanup-namespaces", false, "if true, clean up namespaces after completion")
	command.Flags().StringVar(&args.DestinationType, "destination-type", "", "override to set what to direct requests at; if not specified, the tests will be left as-is; one of "+strings.Join(generator.AllProbeModes, ", "))

	command.Flags().StringSliceVar(&args.Include, "include", []string{}, "include tests with any of these tags; if empty, all tests will be included.  Valid tags:\n"+strings.Join(generator.TagSlice, "\n"))
	command.Flags().StringSliceVar(&args.Exclude, "exclude", []string{generator.TagMultiPeer, generator.TagUpstreamE2E, generator.TagExample}, "exclude tests with any of these tags.  See 'include' field for valid tags")

	command.Flags().BoolVar(&args.Mock, "mock", false, "if true, use a mock kube runner (i.e. don't actually run tests against kubernetes; instead, product fake results")
	command.Flags().BoolVar(&args.DryRun, "dry-run", false, "if true, don't actually do anything: just print out what would be done")

	return command
}

func RunGenerateCommand(args *GenerateArgs) {
	RunVersionCommand()

	utils.DoOrDie(generator.ValidateTags(append(args.Include, args.Exclude...)))

	externalIPs := []string{} // "http://www.google.com"} // TODO make these be IPs?  or not?

	var kubernetes kube.IKubernetes
	if args.Mock || args.DryRun {
		kubernetes = kube.NewMockKubernetes(1.0)
	} else {
		kubeClient, err := kube.NewKubernetesForContext(args.Context)
		utils.DoOrDie(err)
		info, err := kubeClient.ClientSet.ServerVersion()
		utils.DoOrDie(err)
		fmt.Printf("Kubernetes server version: \n%s\n", utils.JsonString(info))
		kubernetes = kubeClient
	}

	serverProtocols := parseProtocols(args.ServerProtocols)

	resources, err := probe.NewDefaultResources(kubernetes, args.ServerNamespaces, args.ServerPods, args.ServerPorts, serverProtocols, externalIPs, args.PodCreationTimeoutSeconds, args.BatchJobs)
	utils.DoOrDie(err)

	interpreterConfig := &connectivity.InterpreterConfig{
		ResetClusterBeforeTestCase:       true,
		KubeProbeRetries:                 args.Retries,
		PerturbationWaitSeconds:          args.PerturbationWaitSeconds,
		VerifyClusterStateBeforeTestCase: true,
		BatchJobs:                        args.BatchJobs,
		IgnoreLoopback:                   args.IgnoreLoopback,
	}
	interpreter := connectivity.NewInterpreter(kubernetes, resources, interpreterConfig)
	printer := &connectivity.Printer{
		Noisy:          args.Noisy,
		IgnoreLoopback: args.IgnoreLoopback,
	}

	zcPod, err := resources.GetPod("z", "c")
	utils.DoOrDie(err)

	testCaseGenerator := generator.NewTestCaseGenerator(args.AllowDNS, zcPod.IP, args.ServerNamespaces, args.Include, args.Exclude)

	testCases := testCaseGenerator.GenerateTestCases()
	fmt.Printf("test cases to run by tag:\n")
	for tag, count := range generator.CountTestCasesByTag(testCases) {
		fmt.Printf("- %s: %d\n", tag, count)
	}
	fmt.Printf("testing %d cases\n\n", len(testCases))
	for i, testCase := range testCases {
		fmt.Printf("test #%d: %s\n - tags: %+v\n", i+1, testCase.Description, strings.Join(testCase.Tags.Keys(), ", "))
	}

	if args.DryRun {
		return
	}

	if args.DestinationType != "" {
		mode, err := generator.ParseProbeMode(args.DestinationType)
		utils.DoOrDie(err)
		for _, testCase := range testCases {
			for _, step := range testCase.Steps {
				step.Probe.Mode = mode
			}
		}
	}

	for i, testCase := range testCases {
		fmt.Printf("starting test case #%d\n", i+1)

		result := interpreter.ExecuteTestCase(testCase)
		utils.DoOrDie(result.Err)

		printer.PrintTestCaseResult(result)
		fmt.Printf("finished policy #%d\n", i+1)
	}

	printer.PrintSummary()

	if args.CleanupNamespaces {
		for _, ns := range args.ServerNamespaces {
			logrus.Infof("cleaning up namespace %s", ns)
			err = kubernetes.DeleteNamespace(ns)
			if err != nil {
				logrus.Warnf("%+v", err)
			}
		}
	}
}
