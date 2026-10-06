from types import SimpleNamespace

from astropods_messaging import mesh_task


def message(platform, platform_data):
    return SimpleNamespace(platform=platform, platform_context=SimpleNamespace(platform_data=platform_data))


def test_mesh_task_reads_the_room_task_its_inputs_and_who_asked():
    task = mesh_task(message("mesh", {
        "mesh_data": '[{"room_task_id":"rt_1","inputs":[{"id":"a1","name":"q3.pdf","content_type":"application/pdf"}]}]',
        "mesh_metadata": '{"on_behalf_of":{"kind":"user","id":"user-1"},"room_task_id":"rt_1"}',
    }))
    assert task.room_task_id == "rt_1"
    assert [(i.id, i.name, i.content_type) for i in task.inputs] == [("a1", "q3.pdf", "application/pdf")]
    assert task.on_behalf_of == {"kind": "user", "id": "user-1"}


def test_mesh_task_is_none_for_a_message_from_another_platform():
    assert mesh_task(message("slack", {})) is None
