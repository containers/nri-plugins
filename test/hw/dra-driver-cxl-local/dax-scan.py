#!/usr/bin/env python3
"""Find aliased memory in the devdax device of a CXL claim.

Usage:
  dax-scan.py [SEED]

Writes its page index and SEED into the first 16 bytes of every 4 KiB page
of the device of the claim (CXL_<KEY>_DAX, CXL_<KEY>_SIZE), then reads
every page back and prints the runs of pages that do not hold what was
written, with the difference between the index they hold and their own
index ("index delta", in pages): a delta of D means that the page was
overwritten by the write to the page D pages later, that is, the two
offsets map to the same memory (aliasing). Exit status 1 if there are
bad pages.
"""
import mmap, os, struct, sys, time
k=[k for k in os.environ if k.endswith("_DAX")][0]; dev=os.environ[k]; size=int(os.environ[k[:-3]+"SIZE"])
P=4096; G=1<<30; n=size//P; seed=int(sys.argv[1],0) if len(sys.argv)>1 else 0x77
m=mmap.mmap(os.open(dev,os.O_RDWR),size,mmap.MAP_SHARED,mmap.PROT_READ|mmap.PROT_WRITE)
t=time.time()
for i in range(n): m[i*P:i*P+16]=struct.pack("<QQ",i,seed)
print("wrote %d pages in %.1fs"%(n,time.time()-t), flush=True)
# summarize bad pages per GiB and what index they hold
runs=[]; cur=None
for i in range(n):
    got,s=struct.unpack_from("<QQ",m,i*P)
    ok = got==i and s==seed
    key = None if ok else (got-i if s==seed else "seed=%#x"%s)
    if cur and cur[2]==key and cur[1]==i-1: cur[1]=i
    else:
        if cur and cur[2] is not None: runs.append(tuple(cur))
        cur=[i,i,key]
if cur and cur[2] is not None: runs.append(tuple(cur))
bad=sum(b-a+1 for a,b,_ in runs)
print("bad pages %d (%.2f GiB) in %d runs"%(bad,bad*P/G,len(runs)))
deltas={}
for a,b,key in runs: deltas[key]=deltas.get(key,0)+b-a+1
for key,cnt in sorted(deltas.items(),key=lambda x:-x[1])[:10]:
    print("  index delta %s: %d pages (%.2f GiB)%s"%(key,cnt,cnt*P/G,(" = %.2f GiB"%(key*P/G)) if isinstance(key,int) else ""))
for a,b,key in runs[:10]:
    print("  bad [%.3f GiB - %.3f GiB) %d pages, holds index delta %s"%(a*P/G,(b+1)*P/G,b-a+1,key))
sys.exit(1 if bad else 0)
