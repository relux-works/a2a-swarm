/* Adapter-test process only. Never an admitted decoder or codec witness. */
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <string.h>
#include <signal.h>

#ifndef MODE
#error define adapter control MODE
#endif
static const unsigned char h[12]={'V','A','P','1',0x80,0x3e,0,0,1,0,16,0};
int main(void) {
  if (MODE == 1 || MODE == 2) {
    pid_t child=0;
    if (MODE == 2) {
      child=fork();
      if (child < 0) return 2;
      if (!child) { for (;;) pause(); }
    }
    FILE *f=fopen("started", "w");
    if (!f) return 2;
    fprintf(f, "%ld %ld\n", (long)getpid(),(long)child); fclose(f);
    for (;;) pause();
  }
  if (MODE == 3) {
    FILE *f=fopen("foreign.keep", "w");
    if (!f) return 2;
    fputs("synthetic foreign sentinel",f); fclose(f);
    return 71;
  }
  if (MODE == 4) {
    fputs("PRIVATE_SENTINEL /host/private\n\033[31m",stderr);
    fwrite(h,1,sizeof(h),stdout); putchar(0); return 71;
  }
  if (MODE == 5) {
    fwrite(h,1,sizeof(h),stdout);
    for (int i=0; i<960001; ++i) if (putchar(0)==EOF) break;
    return 0;
  }
  if (MODE == 6) { fputs("wrong schema",stdout); return 0; }
  if (MODE == 7) { fwrite(h,1,sizeof(h),stdout); putchar(0); putchar(0); return 0; }
  if (MODE == 8) {
    if (getenv("PATH") || getenv("LD_PRELOAD") || getenv("AUDIO_SENTINEL")) return 72;
    struct stat st;
    if (stat("input.ogg",&st) || (st.st_mode&0777)!=0600) return 72;
    if (stat(".",&st) || (st.st_mode&0777)!=0700) return 72;
    if (stat("home",&st) || (st.st_mode&0777)!=0700) return 72;
    char cwd[4096];
    if (!getcwd(cwd,sizeof(cwd))) return 72;
    const char *home=getenv("HOME"), *tmp=getenv("TMPDIR"), *cache=getenv("XDG_CACHE_HOME");
    if (!home || !tmp || !cache || strncmp(home,cwd,strlen(cwd)) ||
        strncmp(tmp,cwd,strlen(cwd)) || strncmp(cache,cwd,strlen(cwd))) return 72;
    fwrite(h,1,sizeof(h),stdout);
    for (int i=0; i<32000; ++i) putchar(0);
    return 0;
  }
  if (MODE == 9) {
    for (int i=0; i<16385; ++i) if (fputc('X',stderr)==EOF) break;
    fflush(stderr);
    fwrite(h,1,sizeof(h),stdout);
    for (int i=0; i<32000; ++i) putchar(0);
    return 0;
  }
  return 72;
}
