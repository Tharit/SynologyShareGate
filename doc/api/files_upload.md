# Context

for every file, the UI first does a 

POST as application/x-www-form-urlencoded;

to sharing/webapi/entry.cgi

with content in the form:

api=SYNO.FileStation.CheckPermission&method=write&version=3&sharing_id=%222kzQERkrf%22&uploader_name=%22<free text user name to be entered in UI>%22&size=694928&filename=%22260312_bcg_logos.zip%22&overwrite=true

needs sharing_sid cookie, and X-Syno-Sharing header

Response in form

{data: {}, success: true}

# Request

POST as multipart/form-data 

to /webapi/entry.cgi?api=SYNO.FileStation.Upload&method=upload&version=2&_sharing_id=%222kzQERkrf%22

needs sharing_sid cookie, and X-Syno-Sharing header

Fields:
sharing_id = 2kzQERkrf
uploader_name = Martin
file = (binary)
size = 694928
mtime = 1773307604231
overwrite = true

# Response

{"success":true}