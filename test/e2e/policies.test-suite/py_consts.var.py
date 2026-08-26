# Helpers for the expressions which "verify" evaluates, shared by every policy
# and topology of this test suite.
#
# These run after the state which "report allowed" collects, so they can refer
# to its variables: cpus, mems, nodes, cores, threads, dies, packages and
# allocations. See "run.sh help pyexec".

def local_mems(ctr, *types):
    """Verify that ctr uses exactly the given memory types of its own package.

    The memory_nodes mapping tells which memory node of a package holds which
    memory type. A topology which has more than one memory type defines it,
    see for instance n6-hbm-cxl/py_consts.var.py.
    """
    package = 0 if packages[ctr] == {"package0"} else 1
    expected = set(memory_nodes[package][t] for t in types)
    assert mems[ctr] == expected, (
        "%s runs in %s, so its %s memory should be %s, but it uses %s" %
        (ctr,
         ",".join(sorted(packages[ctr])),
         ",".join(types),
         ",".join(sorted(expected)),
         ",".join(sorted(mems[ctr]))))
    return True
