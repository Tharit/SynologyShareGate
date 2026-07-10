# Request

POST as application/x-www-form-urlencoded;

to sharing/webapi/entry.cgi

with content in the form:

api=SYNO.FolderSharing.List&method=list&version=2&offset=0&limit=1000&sort_by=%22name%22&sort_direction=%22ASC%22&action=%22enum%22&additional=%5B%22size%22%2C%22owner%22%2C%22time%22%2C%22perm%22%2C%22type%22%2C%22mount_point_type%22%5D&filetype=%22all%22&folder_path=%22%2F02.10.2009%20-%20Tisch%20Evaluation%22&_sharing_id=%22pKGFcZ6A4%22

# Response
{
    "data": {
        "files": [
            {
                "additional": {
                    "mount_point_type": "",
                    "owner": {
                        "gid": 100,
                        "group": "users",
                        "uid": 1026,
                        "user": "martin"
                    },
                    "perm": {
                        "acl": {
                            "append": true,
                            "del": true,
                            "exec": true,
                            "read": true,
                            "write": true
                        },
                        "is_acl_mode": true,
                        "posix": 0
                    },
                    "size": 3359239,
                    "time": {
                        "atime": 1783544561,
                        "crtime": 1506621268,
                        "ctime": 1651956821,
                        "mtime": 1254476266
                    },
                    "type": "JPG"
                },
                "isdir": false,
                "name": "DSC_0997.JPG",
                "path": "/02.10.2009 - Tisch Evaluation/DSC_0997.JPG"
            },
            ...
        ],
        "offset": 0,
        "total": 64
    },
    "success": true
}