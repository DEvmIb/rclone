#!/bin/bash
for i in {1..100}
do
    echo $i|./rclone rcat test:/$(pwgen 1024 1).txt
done