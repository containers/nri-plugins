def cpuset(s):
    """Return a cpuset such as 4-5,8 as a set of CPU names, as in cpus."""
    names = set()
    for r in filter(None, s.split(",")):
        first, _, last = r.partition("-")
        for cpu in range(int(first), int(last or first) + 1):
            names.add("cpu%02d" % cpu)
    return names
