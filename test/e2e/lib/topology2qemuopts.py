#!/usr/bin/env python3


"""topology2qemuopts - convert system structures from JSON to Qemu options

System structures are given in a JSON list of
- NUMA node group definitions and
- CXL structures (optional).

topology2qemuopts outputs Qemu command line parameters that emulate a
system with given structures. Use environment variable
SEPARATED_OUTPUT_VARS=1 to group parameters in separate categories.

NUMA node group definitions:
"mem"                 mem (RAM) size on each NUMA node in this group.
                      The default is "0G".
"nvmem"               nvmem (non-volatile RAM) size on each NUMA node
                      in this group. The default is "0G".
"dimm"                "": the default, memory is there without pc-dimm defined.
                      "plugged": start with cold plugged pc-dimm.
                      "unplugged": start with free slot for hot plug.
                        Add the dimm in Qemu monitor at runtime:
                          device_add pc-dimm,id=dimmX,memdev=memX,node=X
                        or
                          device_add nvdimm,id=nvdimmX,memdev=nvmemX,node=X
"cores"               number of CPU cores on each NUMA node in this group.
                      The default is 0.
"threads"             number of threads on each CPU core.
                      The default is 2.
"nodes"               number of NUMA nodes on each die.
                      The default is 1.
"dies"                number of dies on each package.
                      The default is 1.
"packages"            number of packages.
                      The default is 1.
"cpus-present"        number of logical CPUs present in the system.
                      The default value 0 means "all".

NUMA node distances are defined with following keys:
"dist-all": [[from0to0, from0to1, ...], [from1to0, from1to1, ...], ...]
                      distances from every node to all nodes.
                      The order is the same as in to numactl -H
                      "node distances:" output.
"node-dist": {"node": dist, ...}
                      symmetrical distances from nodes in this group to other
                      nodes.

Distances that apply to all NUMA groups if defined in any:
"dist-same-die": N    the default distance between NUMA nodes on the same die.
"dist-same-package": N the default distance between NUMA nodes on the same package.
"dist-other-package": N  the default distance between NUMA nodes in other packages.

Note that the distance from a node to itself is always 10. The default
distance to a node on the same die is 11, and to other nodes on the
same and different packages is 21.

Example: Each of the first two NUMA groups in the list contains two
NUMA nodes. Each node in the first group includes two CPU cores and 2G
RAM, while nodes in the second group two CPU cores and 1G RAM. The
only NUMA node defined in the third group has 8G of NVRAM, and no CPU.

Every NUMA group with CPU cores adds a package (a socket) to the
configuration, or many identical packages if "packages" > 1.  This
example creates a two-socket system, four CPU cores per package. Note
that CPU cores are divided symmetrically to packages, meaning that
every NUMA group with CPU cores should contain the same number of
cores.

$ ( cat << EOF
[
    {
        "mem": "2G",
        "cores": 2,
        "nodes": 2
    },
    {
        "mem": "1G",
        "cores": 2,
        "nodes": 2
    },
    {
        "nvmem": "8G",
        "node-dist": {"0": 88, "1": 88, "2": 88, "3": 88,
                      "4": 66, "5": 66, "7": 66, "8": 66}
    }
]
EOF
) | python3 topology2qemuopts.py

CXL structures:
"cxl"                 List of host bridge connections.
"hdm-for-passthrough" (optional, next to "cxl" in the same group) true gives
                      every host bridge HDM decoders of its own
                      (pxb-cxl hdm_for_passthrough=on). Without them a host
                      bridge with a single root port passes addresses
                      through, which allows only one CXL region behind
                      the host bridge. The default is false.

                      New host bridge (pxb-cxl) is created for each list
                      of host bridge connections.
                      Each host bridge is attached to the NUMA node
                      corresponding to bridge's index in the list.
                      Each host bridge has its own CXL Fixed Memory Window
                      (cxl-fmw), that is, CXL memories are not interleaved.
                      That is, regions can be created on memory devices
                      independently.

                      Each host bridge connection is a list of root
                      port connections to the host bridge.

                      New root port (cxl-rp) is created for each list of
                      root port connections.
                      Each root port connection is a list of CXL
                      memory devices and CXL switches.

                      CXL memory devices (cxl-type3, volatile-memdev):
                        "mem"     specifies the size.
                        "present" (optional) specifies if the device is
                                  present at vm boot (true, the default)
                                  or if it can be hotplugged later (false).
                                  All devices can be hotremoved.
                        "file"    (optional) absolute path of a file that
                                  backs the memory of the device
                                  (memory-backend-file) instead of anonymous
                                  memory of the qemu process
                                  (memory-backend-ram). Qemu creates the file
                                  and sets its size if the file does not
                                  exist or is empty, but the directory of the
                                  file must exist. Qemus that are given the
                                  same file share the memory.
                        "sn"      (optional) serial number of the device, for
                                  instance "0xc1ae0000". The default is
                                  0xc100e2e0 + index of the device. Give the
                                  same "file" and "sn" to a device in many
                                  VMs to make them see the same device.
                                  Scheme: 0xc100.... boot-time devices of one
                                  VM (the default), 0xc1ae.... shared and
                                  0xc1ee.... unshared devices of a CXL
                                  memory pool.
                        "shared"  (optional) true declares that the device is
                                  meant to be shared with other VMs. Requires
                                  "file". Changes nothing in qemu options:
                                  all CXL memory is mapped with share=on.

                      The qemu memory backend of a device is always created,
                      even if the device is not present at boot. The id of
                      the backend tells how to hotplug the device:
                      be<ram|file>_cxl_memdev<N>__bus_<BUS>__sn_<SN>.

                      CXL switches (cxl-upstream, cxl-downstream):
                        "switch"  specifies a list of CXL memory devices
                                  attached to the switch.

                      Empty slots for hotplugging devices that are not
                      declared in the topology (cxl-rp or cxl-downstream
                      without a memory backend or a device):
                        "pool-slot" specifies the size of the largest device
                                  expected in the slot. It is counted in
                                  total CXL memory, like "mem" of a declared
                                  device, which sets the size of CXL memory
                                  windows.

  CXL Examples:

  # Single 8G CXL memory device attached to NUMA node 0 in a machine with 4G DRAM.
  $ python3 topology2qemuopts.py <<< '[ {"cores":2,"mem":"4G"}, {"cxl": [[{"mem":"8G"}]]} ]'

  # One device attached, two placeholders for hotplugging
  {"cxl": [
      # list of root ports of host bridge 0, in NUMA node 0
      [
          # memory device in root port 0
          {"mem": "256M"}  # cxl_memdev0, is present at boot
      ],
      # list of root ports of host bridge 1, in NUMA node 1
      [
          # CXL switch in root port 1 or host bridge 1
          {"switch": [
              {"mem": "1G", "present": false}, # cxl_memdev1, not present at boot
              {"mem": "1G", "present": false}, # cxl_memdev2, not present at boot
          ]}
      ]
  ]}

  # Two VMs sharing CXL memory. Give the same topology to both VMs. Both qemus
  # map the same file, so the VMs see the same memory device: same size and
  # serial number, same content. Neither VM has the device at boot. Hotplug it
  # with "vm-cxl-hotplug cxl_memdev0" in each VM. Both VMs have also two empty
  # slots (cxl-downstream ports of the switch) for hotplugging devices
  # that are not declared here, for instance by a CXL memory pool emulator.
  # Each device behind the host bridge can be in a region of its own.
  {"hdm-for-passthrough": true,
   "cxl": [
      [
          {"switch": [
              {"mem": "256M", "present": false, "shared": true,
               "file": "/tmp/cxl-pool/static-shared0.raw", "sn": "0xc1ae0000"},
              {"pool-slot": "1G"},
              {"pool-slot": "1G"}
          ]}
      ]
  ]}
"""

import os
import sys
import json

DEFAULT_DIST = 21
DEFAULT_DIST_SAME_PACKAGE = 21
DEFAULT_DIST_SAME_DIE = 11
DEFAULT_DIST_SAME_NODE = 10

separated_output_vars = (os.getenv('SEPARATED_OUTPUT_VARS', 0) == '1')

def error(msg, exitstatus=1):
    sys.stderr.write("topology2qemuopts: %s\n" % (msg,))
    if exitstatus is not None:
        sys.exit(exitstatus)

def siadd(s1, s2):
    if s1.lower().endswith("g") and s2.lower().endswith("g"):
        return str(int(s1[:-1]) + int(s2[:-1])) + "G"
    raise ValueError('supports only sizes in gigabytes, example: 2G')

def sisub(s1, s2):
    if s1.lower().endswith("g") and s2.lower().endswith("g"):
        return str(int(s1[:-1]) - int(s2[:-1])) + "G"
    raise ValueError('supports only sizes in gigabytes, example: 2G')

def validate(numalist):
    if not isinstance(numalist, list):
        raise ValueError('expected list containing dicts, got %s' % (type(numalist,).__name__))
    valid_keys = set(("mem", "nvmem", "dimm",
                      "cores", "threads", "nodes", "dies", "packages",
                      "cpus-present",
                      "node-dist", "dist-all",
                      "dist-other-package", "dist-same-package", "dist-same-die",
                      "cxl", "hdm-for-passthrough"))
    int_range_keys = {'cores': ('>= 0', lambda v: v >= 0),
                      'threads': ('> 0', lambda v: v > 0),
                      'nodes': ('> 0', lambda v: v > 0),
                      'dies': ('> 0', lambda v: v > 0),
                      'packages': ('> 0', lambda v: v > 0),
                      'cpus-present': ('>= 0', lambda v: v >=0)}
    for numalistindex, numaspec in enumerate(numalist):
        for key in numaspec:
            if not key in valid_keys:
                raise ValueError('invalid name %r in node %r' % (key, numaspec))
            if key in ["mem", "nvmem"]:
                val = numaspec.get(key)
                if val == "0":
                    continue
                errmsg = 'invalid %s in node %r, expected string like "2G"' % (key, numaspec)
                if not isinstance(val, str):
                    raise ValueError(errmsg)
                try:
                    siadd(val, "0G")
                except ValueError:
                    raise ValueError(errmsg)
            if key in int_range_keys:
                try:
                    val = int(numaspec[key])
                    if not int_range_keys[key][1](val):
                        raise Exception()
                except:
                    raise ValueError('invalid %s in node %r, expected integer %s' % (key, numaspec, int_range_keys[key][0]))
        if 'threads' in numaspec and int(numaspec.get('cores', 0)) == 0:
            raise ValueError('threads set to %s but "cores" is 0 in node %r' % (numaspec["threads"], numaspec))

def dists(numalist):
    dist_dict = {} # Return value: {sourcenode: {destnode: dist}}, fully defined for all nodes
    sourcenode = -1
    lastsocket = -1
    dist_same_die = DEFAULT_DIST_SAME_DIE
    dist_same_package = DEFAULT_DIST_SAME_PACKAGE
    dist_other_package = DEFAULT_DIST # numalist "dist", if defined
    node_package_die = {} # topology {node: (package, die)}
    dist_matrix = None # numalist "dist_matrix", if defined
    node_node_dist = {} # numalist {sourcenode: {destnode: dist}}, if defined for sourcenode
    lastnode_in_group = -1
    for groupindex, numaspec in enumerate(numalist):
        nodecount = int(numaspec.get("nodes", 1))
        corecount = int(numaspec.get("cores", 0))
        diecount = int(numaspec.get("dies", 1))
        packagecount = int(numaspec.get("packages", 1))
        first_node_in_group = sourcenode + 1
        for package in range(packagecount):
            if nodecount > 0:
                lastsocket += 1
            for die in range(diecount):
                for node in range(nodecount):
                    sourcenode += 1
                    dist_dict[sourcenode] = {}
                    node_package_die[sourcenode] = (lastsocket, die)
        lastnode_in_group = sourcenode + 1
        if "dist" in numaspec:
            dist = numaspec["dist"]
        if "dist-same-die" in numaspec:
            dist_same_die = numaspec["dist-same-die"]
        if "dist-same-package" in numaspec:
            dist_same_package = numaspec["dist-same-package"]
        if "dist-all" in numaspec:
            dist_matrix = numaspec["dist-all"]
        if "node-dist" in numaspec:
            for n in range(first_node_in_group, lastnode_in_group):
                node_node_dist[n] = {int(nodename): value for nodename, value in numaspec["node-dist"].items()}
    if lastnode_in_group < 0:
        raise ValueError('no NUMA nodes found')
    lastnode = lastnode_in_group - 1
    if dist_matrix is not None:
        # Fill the dist_dict directly from dist_matrix.
        # It must cover all distances.
        if len(dist_matrix) != lastnode + 1:
            raise ValueError("wrong dimensions in dist-all %s rows seen, %s expected" % (len(dist_matrix), lastnode))
        for sourcenode, row in enumerate(dist_matrix):
            if len(row) != lastnode + 1:
                raise ValueError("wrong dimensions in dist-all on row %s: %s distances seen, %s expected" % (sourcenode + 1, len(row), lastnode + 1))
            for destnode, source_dest_dist in enumerate(row):
                dist_dict[sourcenode][destnode] = source_dest_dist
    else:
        for sourcenode in range(lastnode + 1):
            for destnode in range(lastnode + 1):
                if sourcenode == destnode:
                    dist_dict[sourcenode][destnode] = DEFAULT_DIST_SAME_NODE
                elif sourcenode in node_node_dist and destnode in node_node_dist[sourcenode]:
                    # User specified explicit node-to-node distance
                    dist_dict[sourcenode][destnode] = node_node_dist[sourcenode][destnode]
                    dist_dict[destnode][sourcenode] = node_node_dist[sourcenode][destnode]
                elif not destnode in dist_dict[sourcenode]:
                    # Set distance based on topology
                    if node_package_die[sourcenode] == node_package_die[destnode]:
                        dist_dict[sourcenode][destnode] = dist_same_die
                    elif node_package_die[sourcenode][0] == node_package_die[destnode][0]:
                        dist_dict[sourcenode][destnode] = dist_same_package
                    else:
                        dist_dict[sourcenode][destnode] = dist_other_package
    return dist_dict

def qemucxlopts(cxl_host_bridges, hdm_for_passthrough=False):
    cxl_objectparams = []      # qemu -object parameters needed for CXL.
    cxl_deviceparams = []      # qemu -device parameters needed for CXL.
    total_mem_sizeM = 0        # total memory in CXL memory devices in megabytes.
    mem_count = 0              # number of memory backend devices.
    port_count = 0             # number of ports (root ports or switches).
    bus_nr = 12                # bus_nr partitions the 0..255 bus number space.
    slot = 0
    chassis = 0xc1             # (slot, chassis) must be unique for each root port.
    serials = set()            # serial numbers of CXL memory devices, must be unique.

    def cxlsizeM(size):
        """Return CXL memory size string ("256M", "1G") in megabytes."""
        if size.endswith("G"):
            return int(size[:-1]) * 1024
        elif size.endswith("M"):
            return int(size[:-1])
        raise ValueError('CXL memory size must be in M or G, got %r' % (size,))

    def cxlserial(device):
        """Return serial number of a CXL memory device with explicit "sn"."""
        try:
            sn = device["sn"]
            if isinstance(sn, str):
                sn = int(sn, 0)
            if not isinstance(sn, int) or isinstance(sn, bool) or not 0 <= sn < 2**64:
                raise ValueError("not a 64-bit unsigned integer")
        except Exception as e:
            raise ValueError('bad "sn" in CXL memory device %s: %s' % (device, e))
        return "0x%x" % (sn,)

    def cxlmemopts(device, bus_id):
        """Create CXL memory device -object and -device that connect it to bus_id."""
        nonlocal mem_count, total_mem_sizeM
        unknown_keys = set(device.keys()) - {"mem", "present", "file", "sn", "shared"}
        if unknown_keys:
            raise ValueError('unsupported key(s) %s in CXL memory device %s' % (", ".join(sorted(unknown_keys)), device))
        mem_size = device["mem"]
        mem_type = "volatile" # non-volatile to be added, possibly as memory device["nvmem"]
        try:
            sizeM = cxlsizeM(mem_size)
        except Exception as e:
            raise Exception("bad memory size in CXL memory device %s: %s" % (device, e))
        mem_file = device.get("file", None)
        if mem_file is not None:
            # The file is relative to the working directory of qemu
            # unless it is absolute, and a comma would end it.
            if not isinstance(mem_file, str) or not os.path.isabs(mem_file) or "," in mem_file:
                raise ValueError('"file" must be an absolute path without commas in CXL memory device %s' % (device,))
        if device.get("shared", False) and mem_file is None:
            raise ValueError('"shared" requires "file" in CXL memory device %s' % (device,))
        if mem_type == "volatile":
            if "sn" in device:
                sn = cxlserial(device)
            else:
                sn = "0xc100%x" % (0xe2e0 + mem_count,)
            if sn in serials:
                raise ValueError('duplicate serial number %s in CXL memory device %s' % (sn, device))
            serials.add(sn)
            memdev_id = f"cxl_memdev{mem_count}"
            # Even if a CXL memory device is not present at boot time, we still create
            # a Qemu memory backend device for it.
            # The backend device id contains all necessary information for hotplugging
            # the CXL memory device later on, and on the other hand, hotremoving and
            # hotplugging the device again, even if it was present at start.
            if mem_file is None:
                backend_id = f"beram_{memdev_id}__bus_{bus_id}__sn_{sn}"
                cxl_objectparams.extend(["-object", f"memory-backend-ram,id={backend_id},share=on,size={mem_size}"])
            else:
                backend_id = f"befile_{memdev_id}__bus_{bus_id}__sn_{sn}"
                cxl_objectparams.extend(["-object", f"memory-backend-file,id={backend_id},share=on,mem-path={mem_file},size={mem_size}"])
            if device.get("present", True):
                cxl_deviceparams.extend(["-device", f"cxl-type3,bus={bus_id},volatile-memdev={backend_id},id={memdev_id},sn={sn}"])
        else:
            raise ValueError('unsupported CXL memory type %r' % (mem_type,))
        total_mem_sizeM += sizeM
        mem_count += 1

    def cxlpoolslotopts(device):
        """Account for an empty slot. The caller has created its port."""
        nonlocal total_mem_sizeM
        if set(device.keys()) != {"pool-slot"}:
            raise ValueError('"pool-slot" cannot be combined with other keys in %s' % (device,))
        try:
            # Count the largest expected device in total CXL memory,
            # so that CXL memory windows are large enough for it.
            # Like the memory of declared devices, this ends up in
            # maxmem, too. Neither needs qemu memory slots (-m slots):
            # cxl-type3 is not a pc-dimm like memory device.
            total_mem_sizeM += cxlsizeM(device["pool-slot"])
        except Exception as e:
            raise Exception("bad size in CXL pool slot %s: %s" % (device, e))

    def cxlswitchopts(device, bus_id):
        """Create CXL switch upstream (to bus_id) and downstream buses"""
        nonlocal port_count, slot
        upstream_id = f"cxlsw_us{bus_id[3:]}"
        cxl_deviceparams.extend(["-device", f"cxl-upstream,bus={bus_id},id={upstream_id}"])
        for downstream_idx, downstream_device in enumerate(device["switch"]):
            downstream_id = f"cxlsw_ds{downstream_idx}_{upstream_id[6:]}"
            cxl_deviceparams.extend(["-device", f"cxl-downstream,port={port_count},bus={upstream_id},id={downstream_id},chassis={chassis},slot={slot}"])
            port_count += 1
            slot += 1
            if "mem" in downstream_device:
                cxlmemopts(downstream_device, downstream_id)
            elif "switch" in downstream_device:
                cxlswitchopts(downstream_device, downstream_id)
            elif "pool-slot" in downstream_device:
                cxlpoolslotopts(downstream_device)
            else:
                raise ValueError('unsupported CXL device in switch %r' % (downstream_device,))

    firmware_targets = []
    for host_bridge, root_ports in enumerate(cxl_host_bridges):
        host_bridge_id = f"cxlhb{host_bridge}"
        pxb_cxl_opts = f"pxb-cxl,bus_nr={bus_nr},bus=pcie.0,id={host_bridge_id},numa_node={host_bridge}"
        if hdm_for_passthrough:
            pxb_cxl_opts += ",hdm_for_passthrough=on"
        cxl_deviceparams.extend(["-device", pxb_cxl_opts])
        firmware_targets.append(host_bridge_id)
        bus_nr += 12
        for root_port, device in enumerate(root_ports):
            root_port_id = f"cxlrp{root_port}{host_bridge_id[3:]}"
            cxl_deviceparams.extend(["-device", f"cxl-rp,port={port_count},bus={host_bridge_id},id={root_port_id},chassis={chassis},slot={slot}"])
            port_count += 1
            slot += 1
            if "mem" in device: # volatile memory directly connected to root port
                cxlmemopts(device, root_port_id)
            elif "switch" in device:
                cxlswitchopts(device, root_port_id)
            elif "pool-slot" in device: # empty root port
                cxlpoolslotopts(device)

    # Firmware size must be larger than total memory size.
    # Round up to nearest 4GB.
    addr_sizeG = ((total_mem_sizeM + 4095) // 4096) * 4
    # Round actual total memory size up to nearest GB for Qemu param.
    total_mem_sizeG = (total_mem_sizeM + 1023) // 1024
    cxl_Mparams = ["-M",
                   ",".join("cxl-fmw.%d.targets.0=%s,cxl-fmw.%d.size=%dG" % (idx, tgt, idx, addr_sizeG)
                            for idx, tgt in enumerate(firmware_targets))]
    return cxl_objectparams, cxl_deviceparams, cxl_Mparams, f"{total_mem_sizeG}G"

def qemuopts(numalist):
    machineparam = "-machine q35,kernel-irqchip=split"
    cpuparam = "-cpu host,x2apic=on"
    numaparams = []
    objectparams = []
    deviceparams = []
    lastnode = -1
    lastcpu = -1
    lastcpupresent = -1
    lastdie = -1
    lastsocket = -1
    lastmem = -1
    lastnvmem = -1
    totalmem = "0G"
    totalnvmem = "0G"
    totalcxlmem = "0G"
    unpluggedmem = "0G"
    pluggedmem = "0G"
    memslots = 0
    groupnodes = {} # groupnodes[NUMALISTINDEX] = (NODEID, ...)
    validate(numalist)

    # Read cpu counts, and "mem" and "nvmem" sizes for all nodes
    # and process "cxl".
    threadcount = -1
    numalist_with_dist = []
    for numalistindex, numaspec in enumerate(numalist):

        # CXL structure
        cxl_spec = numaspec.get("cxl", None)
        if cxl_spec:
            if set(numaspec.keys()) - {"cxl", "hdm-for-passthrough"}:
                raise ValueError("when 'cxl' is defined, no other keys than 'hdm-for-passthrough' are supported in the same group, got %r" % (numaspec.keys(),))
            hdm_for_passthrough = numaspec.get("hdm-for-passthrough", False)
            if not isinstance(hdm_for_passthrough, bool):
                raise ValueError("'hdm-for-passthrough' must be true or false, got %r" % (hdm_for_passthrough,))
            cxl_objectparams, cxl_deviceparams, cxl_Mparams, totalcxlmem = qemucxlopts(cxl_spec, hdm_for_passthrough)
            if cxl_deviceparams:
                objectparams.extend(cxl_objectparams)
                deviceparams.extend(cxl_deviceparams)
                deviceparams.extend(cxl_Mparams)
                memslots += len(objectparams)
                if ",cxl=on" not in machineparam:
                    machineparam += ",cxl=on"
            continue
        if "hdm-for-passthrough" in numaspec:
            raise ValueError("'hdm-for-passthrough' requires 'cxl' in the same group, got %r" % (numaspec,))

        # NUMA node group definition
        numalist_with_dist.append(numaspec)
        nodecount = int(numaspec.get("nodes", 1))
        groupnodes[numalistindex] = tuple(range(lastnode + 1, lastnode + 1 + nodecount))
        corecount = int(numaspec.get("cores", 0))
        if corecount > 0:
            if threadcount < 0:
                # threads per cpu, set only once based on the first cpu-ful numa node
                threadcount = int(numaspec.get("threads", 2))
                threads_set_node = numaspec
            else:
                # threadcount already set, only check that there is no mismatch
                if (numaspec.get("threads", None) is not None and
                    threadcount != int(numaspec.get("threads"))):
                    raise ValueError('all CPUs must have the same number of threads, '
                                     'but %r had %s threads (the default) which contradicts %r' %
                                     (threads_set_node, threadcount, numaspec))
        cpucount = int(numaspec.get("cores", 0)) * threadcount # logical cpus per numa node (cores * threads)
        diecount = int(numaspec.get("dies", 1))
        packagecount = int(numaspec.get("packages", 1))
        cpuspresentcount = int(numaspec.get("cpus-present", 0))
        memsize = numaspec.get("mem", "0")
        memdimm = numaspec.get("dimm", "")
        if memsize != "0":
            memcount = 1
        else:
            memcount = 0
        nvmemsize = numaspec.get("nvmem", "0")
        if nvmemsize != "0":
            nvmemcount = 1
        else:
            nvmemcount = 0
        for package in range(packagecount):
            if nodecount > 0 and cpucount > 0:
                lastsocket += 1
            for die in range(diecount):
                if nodecount > 0 and cpucount > 0:
                    lastdie += 1
                for node in range(nodecount):
                    lastnode += 1
                    currentnumaparams = []
                    for mem in range(memcount):
                        lastmem += 1
                        if memdimm == "":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=membuiltin_%s_node_%s" % (memsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s,memdev=membuiltin_%s_node_%s" % (lastnode, lastmem, lastnode))
                        elif memdimm == "plugged":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=memdimm_%s_node_%s" % (memsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s" % (lastnode,))
                            deviceparams.append("-device")
                            deviceparams.append("pc-dimm,node=%s,id=dimm%s,memdev=memdimm_%s_node_%s" % (lastnode, lastmem, lastmem, lastnode))
                            pluggedmem = siadd(pluggedmem, memsize)
                            memslots += 1
                        elif memdimm == "unplugged":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=memdimm_%s_node_%s" % (memsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s" % (lastnode,))
                            unpluggedmem = siadd(unpluggedmem, memsize)
                            memslots += 1
                        else:
                            raise ValueError("unsupported dimm %r, expected 'plugged' or 'unplugged'" % (memdimm,))
                        totalmem = siadd(totalmem, memsize)
                    for nvmem in range(nvmemcount):
                        lastnvmem += 1
                        lastmem += 1
                        if lastnvmem == 0:
                            machineparam += ",nvdimm=on"
                        # Don't use file-backed nvdimms because the file would
                        # need to be accessible from the govm VM
                        # container. Everything is ram-backed on host for now.
                        if memdimm == "":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=memnvbuiltin_%s_node_%s" % (nvmemsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s,memdev=memnvbuiltin_%s_node_%s" % (lastnode, lastmem, lastnode))
                        elif memdimm == "plugged":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=memnvdimm_%s_node_%s" % (nvmemsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s" % (lastnode,))
                            deviceparams.append("-device")
                            deviceparams.append("nvdimm,node=%s,id=nvdimm%s,memdev=memnvdimm_%s_node_%s" % (lastnode, lastmem, lastmem, lastnode))
                            pluggedmem = siadd(pluggedmem, nvmemsize)
                            memslots += 1
                        elif memdimm == "unplugged":
                            objectparams.append("-object")
                            objectparams.append("memory-backend-ram,size=%s,id=memnvdimm_%s_node_%s" % (nvmemsize, lastmem, lastnode))
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s" % (lastnode,))
                            unpluggedmem = siadd(unpluggedmem, nvmemsize)
                            memslots += 1
                        else:
                            raise ValueError("unsupported dimm %r, expected 'plugged' or 'unplugged'" % (memdimm,))
                        totalnvmem = siadd(totalnvmem, nvmemsize)
                    if cpucount > 0:
                        if not currentnumaparams:
                            currentnumaparams.append("-numa")
                            currentnumaparams.append("node,nodeid=%s" % (lastnode,))
                        currentnumaparams[-1] = currentnumaparams[-1] + (",cpus=%s-%s" % (lastcpu + 1, lastcpu + cpucount))
                        lastcpu += cpucount
                        if cpuspresentcount > 0:
                            lastcpupresent = cpuspresentcount - 1
                        else:
                            lastcpupresent += cpucount
                    numaparams.extend(currentnumaparams)
    node_node_dist = dists(numalist_with_dist)
    for sourcenode in sorted(node_node_dist.keys()):
        for destnode in sorted(node_node_dist[sourcenode].keys()):
            if sourcenode == destnode:
                continue
            numaparams.append("-numa")
            numaparams.append("dist,src=%s,dst=%s,val=%s" % (
                sourcenode, destnode, node_node_dist[sourcenode][destnode]))
    if lastcpu == -1:
        raise ValueError('no CPUs found, make sure at least one NUMA node has "cores" > 0')
    if (lastdie + 1) // (lastsocket + 1) > 1:
        diesparam = ",dies=%s" % ((lastdie + 1) // (lastsocket + 1),)
    else:
        # Don't give dies parameter unless it is absolutely necessary
        # because it requires Qemu >= 5.0.
        diesparam = ""
    smpparam = "-smp cpus=%s,threads=%s%s,sockets=%s,maxcpus=%s" % (lastcpupresent + 1, threadcount, diesparam, lastsocket + 1, lastcpu + 1)
    maxmem = siadd(siadd(totalmem, totalnvmem), totalcxlmem)
    startmem = sisub(sisub(sisub(maxmem, unpluggedmem), pluggedmem), totalcxlmem)
    memparam = "-m size=%s,slots=%s,maxmem=%s" % (startmem, memslots, maxmem)
    if startmem.startswith("0"):
        if pluggedmem.startswith("0"):
            raise ValueError('no memory in any NUMA node')
        raise ValueError("no initial memory in any NUMA node - cannot boot with hotpluggable memory")

    machineparam += ",accel=kvm"

    if separated_output_vars == True:
        return ("MACHINE:" + machineparam + "|" +
                "CPU:" + cpuparam + "|" +
                "SMP:" + smpparam + "|" +
                "MEM:" + memparam + "|" +
                "EXTRA:" +
                ", ".join(map(lambda x: "\"" + x + "\"", numaparams + deviceparams + objectparams))
                )
    else:
        return (machineparam + " " +
            cpuparam + " " +
            smpparam + " " +
            memparam + " " +
            " ".join(numaparams) +
            " " +
            " ".join(deviceparams) +
            " " +
            " ".join(objectparams)
            )

def main(input_file):
    try:
        numalist = json.loads(input_file.read())
    except Exception as e:
        error("error reading JSON: %s" % (e,))
    try:
        print(qemuopts(numalist))
    except Exception as e:
        error("error converting JSON to Qemu opts: %s" % (e,))

if __name__ == "__main__":
    if len(sys.argv) > 1:
        if sys.argv[1] in ["-h", "--help"]:
            print(__doc__)
            sys.exit(0)
        else:
            input_file = open(sys.argv[1])
    else:
        input_file = sys.stdin
    main(input_file)
