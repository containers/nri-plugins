CXL memory sharing: the live demo
=================================
The guide of the demo is docs/memory/cxl/demo.md of the nri-plugins tree:
the stories (local, pool, pool-shared-fs, pool-fs, the wizard and
llm-share), how to prepare a host, the presenter's screen, every step with
what to point at, the latency variant, the results and troubleshooting.

This directory is no test: its setup and teardown are the tests
test98-demo-setup and test99-demo-teardown of n4-cxl-shared-2, which a run
of the suite skips. bin/demo is the entry point; it finds the tree by the
nearest directory above with the go.mod of github.com/containers/
nri-plugins, or NRI_PLUGINS_SRC.

  bin/demo setup local      set up a story (local, pool, pool-shared-fs,
                            pool-fs [ext4|xfs|btrfs], wizard, llm-share)
  bin/demo local            run its steps in the presenter's terminal
  bin/demo commands         the commands of the six terminals
  bin/demo teardown         leave all VMs clean
  bin/demo selftest         the desk check: every view rendered from
                            testdata/, the wizard's checks, the host
                            scripts against stubs; needs no VM

bin/ has the scripts, bin/tools/ the tools in the pods, manifests/ the
pods and claims of the stories, image/ the demo image, testdata/ the
samples of the selftest. run/ and captures/ are the state and the
captures of a demo, out of git.
