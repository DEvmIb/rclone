# this branch is dedicated to my privacer backend WIP

* use mysql for metadata. other DB's can be added easy.
* every chunk has random size. upload same file to other folder has other size
* remote chunks named as numbers. you can use the crypt backend without filename encryption.
* modtime random chosen for remote chunks and original is saved in meta database. when not supported by remote the current stamp will be used.
* files deleted will only removed from metadata and moved to trash
* virtual .trash folder

#### remote folder structur

* example show's 4 copys of the same file 10mb.img

```/tmp/c2/chunks/
/tmp/c2/chunks/b
/tmp/c2/chunks/b/1652861837516626301.0
/tmp/c2/chunks/4
/tmp/c2/chunks/4/1652861837516626301.1
/tmp/c2/chunks/c
/tmp/c2/chunks/c/1652861837516626301.2
/tmp/c2/chunks/a
/tmp/c2/chunks/a/1652861839312559677.0
/tmp/c2/chunks/a/1652861842519131279.0
/tmp/c2/chunks/2
/tmp/c2/chunks/2/1652861839312559677.1
/tmp/c2/chunks/2/1652861840904249693.0
/tmp/c2/chunks/0
/tmp/c2/chunks/0/1652861839312559677.2
/tmp/c2/chunks/f
/tmp/c2/chunks/f/1652861840904249693.1
/tmp/c2/chunks/1
/tmp/c2/chunks/1/1652861840904249693.2
/tmp/c2/chunks/9
/tmp/c2/chunks/9/1652861842519131279.1
```

```-rw-r--r--  1 root root  800093 1972-03-14 09:09:16.000000000 +0100 /tmp/c2/chunks/0/1652861839312559677.2
-rw-r--r--  1 root root 1970538 1982-06-27 08:44:36.000000000 +0200 /tmp/c2/chunks/1/1652861840904249693.2
-rw-r--r--  1 root root 4738231 1970-12-30 07:54:55.000000000 +0100 /tmp/c2/chunks/2/1652861839312559677.1
-rw-r--r--  1 root root 4709032 1976-03-17 00:51:27.000000000 +0100 /tmp/c2/chunks/2/1652861840904249693.0
-rw-r--r--  1 root root 3197886 1996-01-23 14:00:21.000000000 +0100 /tmp/c2/chunks/4/1652861837516626301.1
-rw-r--r--  1 root root 4404672 2008-09-15 15:08:20.000000000 +0200 /tmp/c2/chunks/9/1652861842519131279.1
-rw-r--r--  1 root root 4947436 2000-07-20 07:39:59.000000000 +0200 /tmp/c2/chunks/a/1652861839312559677.0
-rw-r--r--  1 root root 6081088 1989-02-19 09:36:06.000000000 +0100 /tmp/c2/chunks/a/1652861842519131279.0
-rw-r--r--  1 root root 4235511 1973-02-04 15:12:19.000000000 +0100 /tmp/c2/chunks/b/1652861837516626301.0
-rw-r--r--  1 root root 3052363 1983-06-09 02:52:01.000000000 +0200 /tmp/c2/chunks/c/1652861837516626301.2
-rw-r--r--  1 root root 3806190 1970-08-13 22:43:27.000000000 +0100 /tmp/c2/chunks/f/1652861840904249693.1
```