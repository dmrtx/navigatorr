import argparse,subprocess,os,json,time,pathlib,math
parser=argparse.ArgumentParser(description="Reproduce bounded synthetic SDR mode calibration; no library media")
parser.add_argument("--output-dir",required=True)
parser.add_argument("--ffmpeg",default="ffmpeg")
parser.add_argument("--libvmaf-dir",help="Optional directory containing a compatible libvmaf dynamic library")
args=parser.parse_args()
root=pathlib.Path(args.output_dir).resolve();root.mkdir(parents=True,exist_ok=True)
env=os.environ.copy()
if args.libvmaf_dir:
 env['DYLD_LIBRARY_PATH']=args.libvmaf_dir
 env['LD_LIBRARY_PATH']=args.libvmaf_dir
ff=args.ffmpeg
def run(args):
 t=time.monotonic(); p=subprocess.run([ff,'-hide_banner','-loglevel','error','-nostdin','-y']+args,env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=240)
 if p.returncode: raise RuntimeError(p.stderr.decode()[-3000:])
 return time.monotonic()-t
fixtures={
 'motion8':'testsrc2=size=1920x1080:rate=24:duration=2',
 'gradient8':"nullsrc=size=1920x1080:rate=24:duration=2,geq=lum='16+219*X/W':cb=128:cr=128,drawbox=x=100+10*t:y=150:w=200:h=300:color=white:t=3",
 'grain10':"testsrc2=size=1920x1080:rate=24:duration=2,format=yuv420p10le,noise=alls=8:allf=t+u:all_seed=31415"
}
rows=[]
for name,source in fixtures.items():
 depth='yuv420p10le' if name.endswith('10') else 'yuv420p'
 ref=root/(name+'-source.mkv')
 run(['-f','lavfi','-i',source,'-c:v','ffv1','-pix_fmt',depth,str(ref)])
 for q in [20,26,40]:
  cand=root/(name+'-q'+str(q)+'.mkv')
  elapsed=run(['-i',str(ref),'-c:v','libx265','-preset','medium','-crf',str(q),'-pix_fmt',depth,'-x265-params','pools=2:frame-threads=2:log-level=error',str(cand)])
  row={'fixture':name,'quality':q,'encode_seconds':round(elapsed,3),'candidate_bytes':cand.stat().st_size,'reference_bytes':ref.stat().st_size}
  for metric in ['vmaf','cambi']:
   log=root/(name+'-q'+str(q)+'-'+metric+'.json')
   opts='model=version=vmaf_v1.0.16_3d0h' if metric=='vmaf' else 'model=version=vmaf_v0.6.1:feature=name=cambi\\\\:full_ref=true'
   graph=f'[0:v]format=yuv420p10le,setpts=PTS-STARTPTS[d];[1:v]format=yuv420p10le,setpts=PTS-STARTPTS[r];[d][r]libvmaf={opts}:log_fmt=json:log_path={log}:shortest=1:repeatlast=0'
   run(['-i',str(cand),'-i',str(ref),'-filter_complex',graph,'-f','null','-'])
   data=json.loads(log.read_text()); row['libvmaf_version']=data.get('version')
   key='vmaf' if metric=='vmaf' else ('cambi_full_reference' if 'cambi_full_reference' in data['frames'][0]['metrics'] else 'cambi')
   values=[x['metrics'][key] for x in data['frames']]
   row[metric+'_mean']=round(sum(values)/len(values),4); row[metric+'_max']=round(max(values),4)
   if metric=='vmaf': row['vmaf_p5']=round(sorted(values)[max(0,math.ceil(.05*len(values))-1)],4)
  rows.append(row); (root/'measurements.json').write_text(json.dumps(rows,indent=2)); print(row,flush=True)
  run(['-ss','1','-i',str(cand),'-frames:v','1',str(root/(name+'-q'+str(q)+'.png'))])
 run(['-ss','1','-i',str(ref),'-frames:v','1',str(root/(name+'-source.png'))])
print('DONE',flush=True)
