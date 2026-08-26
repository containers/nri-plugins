dram0 = "node0"
dram1 = "node1"
hbm0  = "node2"
hbm1  = "node3"
pmem0 = "node4"
pmem1 = "node5"

# Which memory node of a package holds which memory type. local_mems() of the
# test suite level py_consts.var.py uses this.
memory_nodes = {
    0: {"dram": dram0, "hbm": hbm0, "pmem": pmem0},
    1: {"dram": dram1, "hbm": hbm1, "pmem": pmem1},
}
