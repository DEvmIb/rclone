
# Personal Rclone repo with my patches

for the original repo of rclone goto: [https://github.com/rclone/rclone]

currently rclone patches from me:

* privacer backend goto: [https://github.com/DEvmIb/rclone/tree/test-chunker-split]


* WIP chunkerc - crypt and privacy friendly chunker

    - remote files named sha1sum of filename + chunker data. so no longer long filenames problem on crypt
    - files will automatic splitted at random size. same files uploaded to other folder has other size
    - when supported by remote the modtime is randomized between 1970-now
    - original filename and modtime is saved in metadata
    - status: works but testing testing testing.....

* m2mfs

    - creating an union with all disk avaiable to the system and copy every file only to the most 2 free disks

* cryptdb backend

    - long filenames bigger than 255 will be written to local db (plain files, mysql planning)

#




